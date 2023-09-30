package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/grafov/m3u8"
	"github.com/schollz/progressbar/v3"
)

func concatUrl(base *url.URL, path string) *url.URL {
	if strings.Contains(path, "http") {
		uri, _ := url.Parse(path)
		return uri
	}
	trimmed := base.Path[:strings.LastIndex(base.Path, "/")]
	// url with scheme and domain
	return &url.URL{
		Scheme: base.Scheme,
		Host:   base.Host,
		Path:   trimmed + "/" + path,
	}
}

func Get(ctx context.Context, uri *url.URL) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, uri.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "hls_downloader")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return data, nil
}

type task struct {
	index   int
	segment *m3u8.MediaSegment
}

type finishTask struct {
	task
	fileName string
}

type downloadInput struct {
	variantUrl   *url.URL
	segments     []*m3u8.MediaSegment
	tmpDir       string
	listFile     *os.File
	progressBar  *progressbar.ProgressBar
	numOfWorkers int
}

// downloadSegments downloads the segments in parallel, and writes them to the listFile as they are downloaded.
// download 10 segments at a time
// segments order must be preserved when writing to the listFile
func downloadSegments(ctx context.Context, input *downloadInput) error {
	tasks := make([]task, len(input.segments))
	for i, segment := range input.segments {
		tasks[i] = task{
			index:   i,
			segment: segment,
		}
	}
	finishedTasks := []finishTask{}
	var wg sync.WaitGroup
	sem := make(chan struct{}, input.numOfWorkers)
	for i, tsk := range tasks {
		sem <- struct{}{}
		wg.Add(1)
		go func(i int, tsk task) {
			defer wg.Done()
			defer func() { <-sem }()
			defer input.progressBar.Add(1)
			fName := filepath.Join(input.tmpDir, fmt.Sprintf("%d.ts", i))
			uri := concatUrl(input.variantUrl, tsk.segment.URI)
			data, err := Get(ctx, uri)
			if err != nil {
				panic(err)
			}
			f, err := os.Create(fName)
			if err != nil {
				panic(err)
			}
			defer f.Close()
			if _, err := f.Write(data); err != nil {
				panic(err)
			}
			finishedTasks = append(finishedTasks, finishTask{
				task:     tsk,
				fileName: f.Name(),
			})
		}(i, tsk)
	}
	wg.Wait()
	// sort the finished tasks by index to preserve the segments order
	sort.Slice(finishedTasks, func(i, j int) bool {
		return finishedTasks[i].index < finishedTasks[j].index
	})
	str := ""
	for _, tsk := range finishedTasks {
		str += fmt.Sprintf("file '%s'\n", tsk.fileName)
	}
	if _, err := input.listFile.WriteString(str); err != nil {
		return err
	}
	return nil
}
