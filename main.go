package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log"
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
	u, out              string
	numberOfWorkers     int
	overrideCurrentFile bool
	alwaysHightest      bool
	verbose             bool
)

func main() {
	flag.StringVar(&u, "url", "", "Master playlist direct url (required)")
	flag.StringVar(&out, "o", "", "Output file (mp4 format) (default: timestamp.mp4)")
	flag.IntVar(&numberOfWorkers, "p", 0, "Number of workers, if 0, number of CPU cores will be used")
	flag.BoolVar(&overrideCurrentFile, "f", false, "Override output file if exists")
	flag.BoolVar(&alwaysHightest, "h", true, "Always select highest bitrate variant")
	flag.BoolVar(&verbose, "v", false, "Verbose mode")
	flag.Parse()

	if u == "" {
		flag.Usage()
		return
	}

	// ensure ffmpeg is installed and runnable
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		fmt.Fprintln(os.Stderr, "ffmpeg not found in PATH. Please install ffmpeg: https://ffmpeg.org/download.html")
		os.Exit(1)
	}
	// sanity-check ffmpeg can be executed
	if out, err := exec.Command("ffmpeg", "-version").CombinedOutput(); err != nil {
		fmt.Fprintln(os.Stderr, "ffmpeg was found but failed to run:", err)
		fmt.Fprintln(os.Stderr, "ffmpeg output:")
		fmt.Fprintln(os.Stderr, string(out))
		os.Exit(1)
	}

	uri, err := url.Parse(u)
	if err != nil {
		log.Panicln(err)
	}

	if out == "" {
		now := time.Now()
		out = now.Format("20060102_150405") + ".mp4"
	}

	// validate the output file name
	if _, err := os.Stat(out); err == nil && !overrideCurrentFile {
		log.Panicln("Output file already exists, use -f to override")
	}
	if !strings.HasSuffix(out, ".mp4") {
		log.Panicln("Output file must be a mp4 file")
	}

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT)
	defer signal.Stop(signals)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tmpDir, err := os.MkdirTemp("", "hls_downloader")
	if err != nil {
		log.Panicln(err)
	}
	defer os.RemoveAll(tmpDir)

	if verbose {
		log.Println("Created temporary directory:", tmpDir)
	}

	go func() {
		<-signals
		cancel()
		os.RemoveAll(tmpDir)
		os.Exit(1)
	}()

	log.Println("Fetching playlist...")

	data, err := Get(ctx, uri)
	if err != nil {
		log.Panicln(err)
	}

	buf := bufio.NewReader(strings.NewReader(string(data)))
	p, listType, err := m3u8.DecodeFrom(buf, false)
	if err != nil {
		log.Panicln(err)
	}

	var masterpl *m3u8.MasterPlaylist

	switch listType {
	case m3u8.MEDIA:
		panic("Master playlist expected, media playlist found")
	case m3u8.MASTER:
		masterpl = p.(*m3u8.MasterPlaylist)
	}

	variants := masterpl.Variants
	if len(variants) == 0 {
		log.Panicln("No variants found in master playlist")
	}

	// sort variants by bandwidth
	sort.Slice(variants, func(i, j int) bool {
		return variants[i].VariantParams.Bandwidth > variants[j].VariantParams.Bandwidth
	})

	log.Println("Available Variants:")
	for i, variant := range variants {
		name := variant.VariantParams.Name
		if name == "" {
			name = variant.VariantParams.Resolution
		}
		if name == "" {
			name = fmt.Sprintf("%d", variant.VariantParams.Bandwidth)
		}
		log.Printf("%d: %s\n", i, name)
	}
	var variantId int

	if alwaysHightest {
		variantId = 0
		log.Printf("Automatically selected highest bitrate variant: %d\n", variantId)
	} else {
		fmt.Print("Select variant: ")
		fmt.Scanln(&variantId)
		if variantId < 0 || variantId >= len(variants) {
			panic("Invalid variant id")
		}
	}

	variant := variants[variantId]

	vUrl := concatUrl(uri, variant.URI)
	if verbose {
		log.Println("Fetching variant playlist:", vUrl)
	}

	vPlaylistD, err := Get(ctx, vUrl)
	if err != nil {
		log.Panicln(err)
	}
	vPlaylistBuf := bufio.NewReader(strings.NewReader(string(vPlaylistD)))
	p, _, err = m3u8.DecodeFrom(vPlaylistBuf, false)
	if err != nil {
		log.Panicln(err)
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
		log.Panicln(err)
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
		log.Println("Total time:", time.Since(st))
	}()

	nWorkers := len(segments) / runtime.NumCPU()
	if nWorkers == 0 {
		nWorkers = 1
	}
	if numberOfWorkers > 0 {
		nWorkers = numberOfWorkers
	}
	if verbose {
		log.Printf("Number of workers: %d\n", nWorkers)
	}

	dInput := &downloadInput{
		playlistKey:  mediapl.Key,
		variantUrl:   vUrl,
		segments:     segments,
		tmpDir:       tmpDir,
		listFile:     listF,
		progressBar:  bar,
		numOfWorkers: nWorkers,
	}
	if err := downloadSegments(ctx, dInput); err != nil {
		log.Panicln(err)
	}

	log.Println("Stitching segments...")

	// concat segments using ffmpeg
	rawArgs := fmt.Sprintf("-v error -y -f concat -safe 0 -i %s -c copy %s", listF.Name(), out)
	output, err := exec.CommandContext(
		ctx,
		"ffmpeg",
		strings.Split(rawArgs, " ")...).CombinedOutput()
	if err != nil {
		log.Println("FFmpeg Error:", string(output))
		log.Panicln(err)
	}

	log.Println("Done!, output file:", out)
}
