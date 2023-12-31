package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/grafov/m3u8"
	"github.com/schollz/progressbar/v3"
)

var (
	u, out          string
	numberOfWorkers int
	verbose         bool
)

func main() {
	flag.StringVar(&u, "url", "", "Master playlist direct url (required)")
	flag.StringVar(&out, "o", "", "Output file (required)")
	flag.IntVar(&numberOfWorkers, "p", 0, "Number of workers")
	flag.BoolVar(&verbose, "v", false, "Verbose mode")
	flag.Parse()

	fmt.Print(`-----------------------------------------------
A simple HLS downloader written in Golang.
This tool is not intended to be used for piracy.
Use it at your own risk.

Version: 0.0.5
By: Ahmed M. Abouelkher
-----------------------------------------------
`)
	if u == "" || out == "" {
		flag.Usage()
		return
	}
	uri, err := url.Parse(u)
	if err != nil {
		panic(err)
	}
	// validate the output file name
	if _, err := os.Stat(out); err == nil {
		panic("Output file already exists")
	}
	if !strings.HasSuffix(out, ".mp4") {
		panic("Output file must be a mp4 file")
	}

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT)
	defer signal.Stop(signals)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tmpDir, err := os.MkdirTemp("", "hls_downloader")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(tmpDir)

	go func() {
		<-signals
		cancel()
		os.RemoveAll(tmpDir)
		os.Exit(1)
	}()

	fmt.Println("Fetching playlist...")

	data, err := Get(ctx, uri)
	if err != nil {
		panic(err)
	}
	buf := bufio.NewReader(strings.NewReader(string(data)))
	p, listType, err := m3u8.DecodeFrom(buf, false)
	if err != nil {
		panic(err)
	}

	var masterpl *m3u8.MasterPlaylist

	switch listType {
	case m3u8.MEDIA:
		panic("Master playlist expected, media playlist found")
	case m3u8.MASTER:
		masterpl = p.(*m3u8.MasterPlaylist)
	}

	variants := masterpl.Variants
	// sort variants by bandwidth
	sort.Slice(variants, func(i, j int) bool {
		return variants[i].VariantParams.Bandwidth > variants[j].VariantParams.Bandwidth
	})

	fmt.Println("Available Variants:")
	for i, variant := range variants {
		name := variant.VariantParams.Name
		if name == "" {
			name = variant.VariantParams.Resolution
		}
		if name == "" {
			name = fmt.Sprintf("%d", variant.VariantParams.Bandwidth)
		}
		fmt.Printf("%d: %s\n", i, name)
	}
	var variantId int
	fmt.Print("Select variant: ")
	fmt.Scanln(&variantId)
	if variantId < 0 || variantId >= len(variants) {
		panic("Invalid variant id")
	}
	variant := variants[variantId]
	vUrl := concatUrl(uri, variant.URI)
	if verbose {
		fmt.Println("Fetching variant playlist:", vUrl)
	}

	vPlaylistD, err := Get(ctx, vUrl)
	if err != nil {
		panic(err)
	}
	vPlaylistBuf := bufio.NewReader(strings.NewReader(string(vPlaylistD)))
	p, _, err = m3u8.DecodeFrom(vPlaylistBuf, false)
	if err != nil {
		panic(err)
	}

	mediapl := p.(*m3u8.MediaPlaylist)
	segments := []*m3u8.MediaSegment{}
	for _, segment := range mediapl.Segments {
		if segment == nil {
			break
		}
		segments = append(segments, segment)
	}

	listF, err := os.CreateTemp(tmpDir, "list")
	if err != nil {
		panic(err)
	}
	defer listF.Close()

	// bar := progressbar.Default(int64(len(segments)), "Downloading segments")
	bar := progressbar.NewOptions64(
		int64(len(segments)),
		progressbar.OptionSetDescription("Downloading segments"),
		progressbar.OptionSetWriter(os.Stderr),
		progressbar.OptionThrottle(65*time.Millisecond),
		progressbar.OptionShowCount(),
		progressbar.OptionClearOnFinish(),
		progressbar.OptionSetElapsedTime(true),
		progressbar.OptionSetPredictTime(false),
		progressbar.OptionFullWidth(),
	)

	st := time.Now()
	defer func() {
		fmt.Println("Total time:", time.Since(st))
	}()

	nWorkers := len(segments) / runtime.NumCPU()
	if nWorkers == 0 {
		nWorkers = 1
	}
	if numberOfWorkers > 0 {
		nWorkers = numberOfWorkers
	}
	if verbose {
		fmt.Printf("Number of workers: %d\n", nWorkers)
	}

	dInput := &downloadInput{
		variantUrl:   vUrl,
		segments:     segments,
		tmpDir:       tmpDir,
		listFile:     listF,
		progressBar:  bar,
		numOfWorkers: nWorkers,
	}
	if err := downloadSegments(ctx, dInput); err != nil {
		panic(err)
	}

	fmt.Println("Stitching segments...")

	// concat segments using ffmpeg
	rawArgs := fmt.Sprintf("-v error -y -f concat -safe 0 -i %s -c copy %s", listF.Name(), out)
	output, err := exec.CommandContext(
		ctx,
		"ffmpeg",
		strings.Split(rawArgs, " ")...).CombinedOutput()
	if err != nil {
		fmt.Println("FFmpeg Error:", string(output))
		panic(err)
	}

	fmt.Println("Done!, output file:", out)
}
