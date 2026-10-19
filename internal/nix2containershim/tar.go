package nix2containershim

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const copyBufferSize = 32 * 1024

func writeRootfs(img *Image, dest io.Writer) error {
	allPathsOnly := true
	var allPaths []StorePath
	for _, layer := range img.Layers {
		if layer.LayerPath != "" {
			allPathsOnly = false
			break
		}
		allPaths = append(allPaths, layer.Paths...)
	}

	if allPathsOnly {
		reader := tarPaths(allPaths)
		defer func() { _ = reader.Close() }()

		_, err := io.Copy(dest, reader)
		return err
	}

	tw := tar.NewWriter(dest)
	defer func() { _ = tw.Close() }()

	for _, layer := range img.Layers {
		err := copyLayerBlob(tw, layer)
		if err != nil {
			return err
		}
	}

	return tw.Close()
}

func copyLayerBlob(tw *tar.Writer, layer Layer) error {
	var rc io.ReadCloser
	var err error

	if layer.LayerPath != "" {
		rc, err = os.Open(layer.LayerPath)
		if err != nil {
			return fmt.Errorf("opening layer path %s: %w", layer.LayerPath, err)
		}
	} else if len(layer.Paths) > 0 {
		rc = tarPaths(layer.Paths)
	} else {
		return nil
	}
	defer func() { _ = rc.Close() }()

	var r io.Reader = rc
	if strings.HasSuffix(layer.LayerPath, ".gz") || strings.Contains(layer.MediaType, "gzip") {
		gzr, err := gzip.NewReader(rc)
		if err != nil {
			return fmt.Errorf("decompressing layer gzip: %w", err)
		}
		defer func() { _ = gzr.Close() }()
		r = gzr
	}

	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("reading layer tar entry: %w", err)
		}

		err = tw.WriteHeader(hdr)
		if err != nil {
			return fmt.Errorf("writing tar header: %w", err)
		}

		_, err = io.Copy(tw, tr) //nolint:gosec
		if err != nil {
			return fmt.Errorf("copying tar entry data: %w", err)
		}
	}
	return nil
}

type tarArchiveReader struct {
	pipeReader *io.PipeReader
	done       chan struct{}
}

func (tar *tarArchiveReader) Read(p []byte) (n int, err error) {
	return tar.pipeReader.Read(p)
}

func (tar *tarArchiveReader) Close() error {
	err := tar.pipeReader.Close()
	<-tar.done
	return err
}

func tarPaths(paths []StorePath) io.ReadCloser {
	pipeReader, pipeWriter := io.Pipe()
	done := make(chan struct{})

	go func() {
		defer close(done)
		tw := tar.NewWriter(pipeWriter)

		buf := make([]byte, copyBufferSize)

		graph := initGraph()
		for _, sp := range paths {
			err := addFileToGraph(graph, sp.Path, sp.Options)
			if err != nil {
				_ = pipeWriter.CloseWithError(err)
				return
			}
		}
		sanitizeGraph(graph)

		err := walkGraph(graph, "", func(srcPath, dstPath string, options *PathOptions) error {
			return appendFileToTar(tw, buf, srcPath, dstPath, options)
		})
		if err != nil {
			_ = pipeWriter.CloseWithError(err)
			return
		}

		err = tw.Close()
		if err != nil {
			_ = pipeWriter.CloseWithError(err)
			return
		}
		_ = pipeWriter.Close()
	}()

	return &tarArchiveReader{
		pipeReader: pipeReader,
		done:       done,
	}
}

type fileInGraph struct {
	srcPath  string
	options  *PathOptions
	children map[string]*fileInGraph
}

func initGraph() *fileInGraph {
	return &fileInGraph{
		srcPath:  "",
		children: make(map[string]*fileInGraph),
	}
}

func addFileToGraph(graph *fileInGraph, fpath string, options *PathOptions) error {
	tarPath := filePathToTarPath(fpath, options)
	parts := splitPath(tarPath)

	cur := graph
	for i, part := range parts {
		if part == "" {
			continue
		}
		child, exists := cur.children[part]
		if !exists {
			child = &fileInGraph{
				srcPath:  "",
				children: make(map[string]*fileInGraph),
			}
			cur.children[part] = child
		}
		if i == len(parts)-1 {
			child.srcPath = fpath
			child.options = options
		}
		cur = child
	}

	info, err := os.Lstat(fpath)
	if err != nil {
		return err
	}
	if info.IsDir() {
		entries, err := os.ReadDir(fpath)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			subPath := filepath.Join(fpath, entry.Name())
			err = addFileToGraph(graph, subPath, options)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func sanitizeGraph(graph *fileInGraph) {
	for k, v := range graph.children {
		unhacked := unhackNixCaseHack(k)
		if unhacked != k {
			delete(graph.children, k)
			graph.children[unhacked] = v
		}
		sanitizeGraph(v)
	}
}

func walkGraph(graph *fileInGraph, prefix string, fn func(srcPath, dstPath string, options *PathOptions) error) error {
	keys := make([]string, 0, len(graph.children))
	for k := range graph.children {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		child := graph.children[k]
		dstPath := filepath.Join(prefix, k)

		if child.srcPath != "" {
			err := fn(child.srcPath, dstPath, child.options)
			if err != nil {
				return err
			}
		} else if len(child.children) > 0 {
			err := createDirectory(dstPath, fn)
			if err != nil {
				return err
			}
		}

		err := walkGraph(child, dstPath, fn)
		if err != nil {
			return err
		}
	}
	return nil
}

func createDirectory(dstPath string, fn func(srcPath, dstPath string, options *PathOptions) error) error {
	return fn("", dstPath, nil)
}

func appendFileToTar(tw *tar.Writer, buf []byte, srcPath, dstPath string, options *PathOptions) error {
	var info os.FileInfo
	var link string
	var err error

	if srcPath != "" {
		info, err = os.Lstat(srcPath)
		if err != nil {
			return fmt.Errorf("lstat %s: %w", srcPath, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			link, err = os.Readlink(srcPath)
			if err != nil {
				return fmt.Errorf("readlink %s: %w", srcPath, err)
			}
		}
	}

	var hdr *tar.Header
	if info != nil {
		hdr, err = tar.FileInfoHeader(info, link)
		if err != nil {
			return fmt.Errorf("generating tar header for %s: %w", srcPath, err)
		}
	} else {
		hdr = &tar.Header{
			Typeflag: tar.TypeDir,
			Mode:     0o755,
		}
	}

	cleanDst := strings.TrimPrefix(filepath.Clean(dstPath), "/")
	if cleanDst == "" {
		return nil
	}
	if info != nil && info.IsDir() {
		cleanDst += "/"
	}
	hdr.Name = cleanDst

	if options != nil {
		for _, perms := range options.Perms {
			re := regexp.MustCompile(perms.Regex)
			if re.MatchString(srcPath) {
				hdr.Uid = perms.UID
				hdr.Gid = perms.GID
				if perms.Uname != "" {
					hdr.Uname = perms.Uname
				}
				if perms.Gname != "" {
					hdr.Gname = perms.Gname
				}
				if perms.Mode != "" {
					_, err = fmt.Sscanf(perms.Mode, "%o", &hdr.Mode)
					if err != nil {
						return err
					}
				}
				if perms.OrMode != "" {
					or, err := strconv.ParseUint(perms.OrMode, 8, 12)
					if err != nil {
						return fmt.Errorf("invalid orMode %q: %w", perms.OrMode, err)
					}
					hdr.Mode |= int64(or)
				}
			}
		}
	}

	epoch := time.Date(1970, 1, 1, 0, 0, 1, 0, time.UTC)
	hdr.ModTime = epoch
	hdr.AccessTime = epoch
	hdr.ChangeTime = epoch

	err = tw.WriteHeader(hdr)
	if err != nil {
		return fmt.Errorf("writing tar header for %s: %w", dstPath, err)
	}

	if link == "" && info != nil && !info.IsDir() {
		file, err := os.Open(srcPath)
		if err != nil {
			return fmt.Errorf("opening file %s: %w", srcPath, err)
		}
		defer func() { _ = file.Close() }()

		_, err = io.CopyBuffer(tw, struct{ io.Reader }{file}, buf)
		if err != nil {
			return fmt.Errorf("copying file %s to tar: %w", srcPath, err)
		}
	}
	return nil
}

func unhackNixCaseHack(filename string) string {
	caseHackSuffix := "~nix~case~hack~"
	splited := strings.Split(filename, caseHackSuffix)
	if len(splited) == 2 {
		idx, err := strconv.ParseInt(splited[1], 10, 64)
		if err == nil && idx >= 1 {
			return splited[0]
		}
	}
	return filename
}

func splitPath(path string) []string {
	cleaned := filepath.Clean(path)
	parts := strings.Split(cleaned, "/")
	if len(parts) == 2 && parts[0] == "" && parts[1] == "" {
		return []string{""}
	}
	return parts
}

func filePathToTarPath(fpath string, options *PathOptions) string {
	tarPath := fpath
	if options != nil && options.Rewrite != nil && options.Rewrite.Regex != "" {
		re := regexp.MustCompile(options.Rewrite.Regex)
		tarPath = re.ReplaceAllString(fpath, options.Rewrite.Repl)
	}
	return tarPath
}
