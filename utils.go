package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/grafov/m3u8"
	"github.com/schollz/progressbar/v3"
)

var (
	EmptySegmentError = fmt.Errorf("empty segment")
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

func DownloadSegment(ctx context.Context, uri *url.URL, fileName string) error {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	data, err := Get(ctx, uri)
	if err != nil {
		return err
	}
	if len(data) == 0 {
		return EmptySegmentError
	}
	f, err := os.Create(fileName)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		return err
	}
	return nil
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

func downloadSegments(ctx context.Context, input *downloadInput) error {
	tasks := make([]task, len(input.segments))
	for i, segment := range input.segments {
		tasks[i] = task{
			index:   i,
			segment: segment,
		}
	}
	// thread-safe map to store the finished tasks
	cFinishedTasks := sync.Map{}
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
			err := backoff.Retry(func() error {
				return DownloadSegment(ctx, uri, fName)
			}, backoff.WithContext(backoff.NewExponentialBackOff(), ctx))
			if err != nil {
				if errors.Is(err, context.Canceled) {
					return
				}
				fmt.Printf("download segment %d failed: %s\n", i, err)
				return
			}
			cFinishedTasks.Store(i, finishTask{
				task:     tsk,
				fileName: fName,
			})
		}(i, tsk)
	}
	wg.Wait()

	finishedTasks := make([]finishTask, len(tasks))
	cFinishedTasks.Range(func(key, value interface{}) bool {
		finishedTasks[key.(int)] = value.(finishTask)
		return true
	})

	retryCount := 5

sort_tasks:
	// sort the finished tasks by index to preserve the segments order
	sort.Slice(finishedTasks, func(i, j int) bool {
		return finishedTasks[i].index < finishedTasks[j].index
	})
	// validate the segments order
	for i, tsk := range finishedTasks {
		if tsk.index != i {
			if retryCount == 0 {
				return fmt.Errorf(
					"download segments failed, %d against %d, %d retries exceeded",
					i,
					tsk.index,
					retryCount,
				)
			}
			retryCount--
			goto sort_tasks
		}
	}
	str := ""
	for _, tsk := range finishedTasks {
		str += fmt.Sprintf("file '%s'\n", tsk.fileName)
	}
	if _, err := input.listFile.WriteString(str); err != nil {
		return err
	}
	return nil
}
