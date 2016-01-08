package main

import (
	"encoding/gob"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"
)

type Download struct {
	position   int64
	size       int64
	segmentNum int
}

type ProgressUpdate struct {
	threadnum  uint
	downloaded int64
}

type ProgressInfo struct {
	Length      int64
	SegmentSize int64
	segments    []bool
}

const (
	maxSegments     = 50000
	finishedSegment = 0x59
	baseOffset      = 256
)

var client *http.Client

func download_segments(threadnum uint, url string, instructions chan Download, updates chan ProgressUpdate,
	file *os.File, progressFile *os.File) {

	finished := true
	buf := make([]byte, 8192)
	total := int64(0)
	errorCount := int64(0)

	var down Download
	for {
		if finished {
			down = <-instructions
			if down.size == 0 {
				break
			}
			finished = false
		}

		req, err := http.NewRequest("GET", url, nil)
		if err != nil {
			fmt.Fprintln(os.Stderr, "Failed to create request")
			os.Exit(1)
		}

		req.Header.Add("Range", fmt.Sprintf("bytes=%d-%d", down.position, down.position+down.size-1))

		resp, err := client.Do(req)
		if err != nil || (resp.StatusCode != 206 && resp.StatusCode != 200) {
			errorCount++
			if errorCount > 3 {
				fmt.Fprintln(os.Stderr, "Failed to run GET too many times. Check network connection?")
				os.Exit(1)
			} else {
				continue
			}
		}

		errorCount = 0

		read := int64(0)

		for read < down.size {
			n, err := resp.Body.Read(buf[:])
			if n == 0 && err != nil {
				break
			}

			file.WriteAt(buf[:n], down.position+read)
			read += int64(n)
			total += int64(n)
			updates <- ProgressUpdate{threadnum: threadnum, downloaded: total}
		}

		if read >= down.size {
			finished = true
			buf[0] = finishedSegment
			progressFile.WriteAt(buf[:1], baseOffset+int64(down.segmentNum))
		} else {
			down.position += read
			down.size -= read
		}
	}
}

func print_progress(updates chan ProgressUpdate, numThreads uint, quiet bool) {
	if quiet {
		for update := <-updates; update.downloaded >= 0; {
			update = <-updates
		}
		return
	}

	fmt.Print("\n")

	startTime := time.Now()
	counts := make([]int64, numThreads)
	var total int64 = 0
	for {
		update := <-updates
		if update.downloaded < 0 {
			break
		}
		result := float64(update.downloaded) / 1000 / float64(time.Since(startTime)/time.Second)
		total += update.downloaded - counts[update.threadnum]
		counts[update.threadnum] = update.downloaded

		fmt.Printf("\x1b[F\x1b[%dG%10.2fkB\x1b[%dG%10.2fMB\n",
			update.threadnum*15, result, numThreads*15, float64(total)/1e6)
	}
}

func findFileLength(url string) int64 {
	resp, err := client.Head(url)

	if err != nil || resp.StatusCode != 200 || resp.ContentLength == -1 {
		fmt.Fprintf(os.Stderr, "Failed to get file length: (Status) %s (Length) %d (Err) %s\n", resp.Status, resp.ContentLength, err)
		os.Exit(1)
	}

	return resp.ContentLength
}

func countTrue(arr []bool) int {
	count := 0
	for _, b := range arr {
		if b {
			count++
		}
	}
	return count
}

func findSegmentCount(length, segmentSize int64) int64 {
	return (length + segmentSize - 1) / segmentSize
}

func setupProgressFile(filename string, length, segmentSize int64, forceClean bool, quiet bool) (file *os.File,
	info ProgressInfo, clean bool, err error) {

	if !forceClean {
		info, err = readProgressInfo(filename)
		clean = err != nil || length != info.Length ||
			findSegmentCount(length, info.SegmentSize) != int64(len(info.segments))
	} else {
		clean = true
	}

	if clean {
		segmentCount := findSegmentCount(length, segmentSize)
		file, err = beginProgressFile(filename, length, segmentSize)
		if err != nil {
			return
		}
		info.Length = length
		info.SegmentSize = segmentSize
		info.segments = make([]bool, segmentCount)
	} else {
		if !quiet {
			segmentCount := findSegmentCount(info.Length, info.SegmentSize)
			finishedSegments := countTrue(info.segments)
			fmt.Printf("Resuming file download %d/%d\n", finishedSegments, segmentCount)
		}
		file, err = os.OpenFile(filename, os.O_WRONLY|os.O_APPEND, 0666)
		if err != nil {
			fmt.Print(err)
			return
		}
	}

	return
}

func beginProgressFile(filename string, length, segmentSize int64) (file *os.File, err error) {

	file, err = os.Create(filename)
	if err != nil {
		return
	}

	header := ProgressInfo{Length: length, SegmentSize: segmentSize}

	if n, err := file.WriteString("MULD"); n != 4 || err != nil {
		return nil, err
	}

	encoder := gob.NewEncoder(file)
	if err = encoder.Encode(&header); err != nil {
		return nil, err
	}

	return
}

func readProgressInfo(filename string) (info ProgressInfo, err error) {
	file, err := os.Open(filename)
	defer file.Close()

	if err != nil {
		return
	}

	var buf [8192]byte
	n, err := file.Read(buf[:4])
	if n != 4 || string(buf[:4]) != "MULD" {
		err = errors.New("invalid magic number")
		return
	}

	decoder := gob.NewDecoder(file)
	if err = decoder.Decode(&info); err != nil {
		return
	}

	segmentCount := findSegmentCount(info.Length, info.SegmentSize)
	if segmentCount > maxSegments {
		err = errors.New("too many segments")
		return
	}

	info.segments = make([]bool, segmentCount)

	file.Seek(baseOffset, os.SEEK_SET)
	for i := int64(0); i < segmentCount; {
		n, err = file.Read(buf[:])
		numRead := int64(n)
		if numRead == 0 && err != nil {
			break
		}

		for j := int64(0); j < numRead && i+j < segmentCount; j++ {
			info.segments[i+j] = (buf[j] == finishedSegment)
		}
		i += numRead
	}

	err = nil

	return
}

func download(url string, filename string, numThreads uint, quiet bool) {
	client = &http.Client{}

	length := findFileLength(url)

	progressFilename := fmt.Sprintf("%s.multidownload", filename)
	defer os.Remove(progressFilename)
	if !quiet {
		fmt.Printf("Download file %.2fMB\n", float64(length)/1e6)
	}

	fileinfo, err := os.Stat(filename)
	if _, progerr := os.Stat(progressFilename); err == nil && fileinfo.Size() == length && progerr != nil {
		if !quiet {
			fmt.Println("File already downloaded")
		}
		return
	}

	progressFile, info, truncate, err := setupProgressFile(progressFilename, length, int64(1000000), err != nil, quiet)

	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to setup progress file: %s\n", err)
		os.Exit(1)
	}
	defer progressFile.Close()

	outopts := os.O_RDWR|os.O_APPEND|os.O_CREATE
	if truncate {
		outopts |= os.O_TRUNC
	}
	file, err := os.OpenFile(filename, outopts, 0666)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Unable to open output file: %s\n", err)
		os.Exit(1)
	}
	defer file.Close()

	instructions := make(chan Download)
	updates := make(chan ProgressUpdate)

	for i := uint(0); i < numThreads; i++ {
		go download_segments(i, url, instructions, updates, file, progressFile)
	}

	go print_progress(updates, numThreads, quiet)

	for seg := 0; seg < len(info.segments); seg++ {
		if info.segments[seg] {
			continue
		}

		left := info.SegmentSize
		pos := int64(seg) * info.SegmentSize
		if pos+info.SegmentSize > info.Length {
			left = info.Length - pos
		}

		instructions <- Download{position: pos, size: left, segmentNum: seg}
	}

	for i := uint(0); i < numThreads; i++ {
		instructions <- Download{position: info.Length, size: 0}
	}
}

func main() {
	outputPtr := flag.String("o", "video.mp4", "output file")
	numThreadsPtr := flag.Uint("n", 4, "number of threads")
	quietPtr := flag.Bool("q", false, "quiet output. only errors displayed")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage of multidown: multidown [-o outputfile] [-n numthreads] url\n")
		flag.PrintDefaults()
	}

	flag.Parse()

	if flag.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "Need to specify a url to download")
		os.Exit(1)
	} else if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "Only one url will download at a time")
		os.Exit(1)
	}

	if *numThreadsPtr == 0 {
		fmt.Fprintln(os.Stderr, "Running with zero threads means nothing will download")
		os.Exit(1)
	}

	download(flag.Arg(0), *outputPtr, *numThreadsPtr, *quietPtr)
}
