package main

import (
	"encoding/gob"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"
)

type Download struct {
	position   int64
	size       int64
	segmentNum int64
}

type ProgressUpdate struct {
	threadnum  uint
	downloaded int64
}

type ProgressInfoHeader struct {
	Length      int64
	SegmentSize int64
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
			log.Fatal("Failed to create request")
			instructions <- down
			break
		}

		req.Header.Add("Range", fmt.Sprintf("bytes=%d-%d", down.position, down.position+down.size-1))

		resp, err := client.Do(req)
		if err != nil || resp.StatusCode != 206 {
			errorCount++
			if errorCount > 3 {
				log.Fatal("Failed to run GET too many times")
				break
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
			progressFile.WriteAt(buf[:1], baseOffset+down.segmentNum)
		} else {
			down.position += read
			down.size -= read
		}
	}
}

func print_progress(updates chan ProgressUpdate, numThreads uint) {
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
		log.Fatalf("Failed to get file length %s (%d): %s", resp.Status, resp.ContentLength, err)
	}

	return resp.ContentLength
}

func setupProgressFile(filename string, length, increment int64, forceClean bool) (file *os.File, segmentSize, segmentCount int64, segments []bool, err error) {

	var clean bool = true

	segmentSize = increment

	if !forceClean {
		infoLength, infoSegmentSize, infoSegments, err := readProgressInfo(filename)
		if err != nil {
			clean = true
		} else if length != infoLength {
			clean = true
		} else {
			segments = infoSegments
			segmentSize = infoSegmentSize
			clean = false
		}
	} else {
		clean = true
	}

	segmentCount = (length + segmentSize - 1) / segmentSize

	if clean {
		file, err = beginProgressFile(filename, length, segmentSize)
		if err != nil {
			return
		}
		segments = make([]bool, segmentCount)
	} else {
		finishedSegments := 0
		for seg := int64(0); seg < segmentCount; seg++ {
			if segments[seg] {
				finishedSegments++
			}
		}
		fmt.Printf("Resuming file download %d/%d\n", finishedSegments, segmentCount)
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

	header := ProgressInfoHeader{Length: length, SegmentSize: segmentSize}

	if n, err := file.WriteString("MULD"); n != 4 || err != nil {
		return nil, err
	}

	encoder := gob.NewEncoder(file)
	if err = encoder.Encode(&header); err != nil {
		return nil, err
	}

	return
}

func readProgressInfo(filename string) (length, segmentSize int64, segments []bool, err error) {
	file, err := os.Open(filename)
	defer file.Close()

	if err != nil {
		return
	}
	var header ProgressInfoHeader

	var buf [8192]byte
	n, err := file.Read(buf[:4])
	if n != 4 || string(buf[:4]) != "MULD" {
		err = errors.New("invalid magic number")
		return
	}

	decoder := gob.NewDecoder(file)
	if err = decoder.Decode(&header); err != nil {
		return
	}
	length, segmentSize = header.Length, header.SegmentSize

	segmentCount := (length + segmentSize - 1) / segmentSize
	if segmentCount > maxSegments {
		err = errors.New("too many segments")
		return
	}

	segments = make([]bool, segmentCount)

	file.Seek(baseOffset, os.SEEK_SET)
	for i := int64(0); i < segmentCount; {
		n, err = file.Read(buf[:])
		numRead := int64(n)
		if numRead == 0 && err != nil {
			break
		}

		for j := int64(0); j < numRead && i+j < segmentCount; j++ {
			segments[i+j] = (buf[j] == finishedSegment)
		}
		i += numRead
	}

	err = nil

	return
}

func download(url string, filename string, numThreads uint) {
	client = &http.Client{}

	length := findFileLength(url)

	progressFilename := fmt.Sprintf("%s.multidownload", filename)
	defer os.Remove(progressFilename)
	fmt.Printf("Download file %.2fMB\n", float64(length)/1e6)

	fileinfo, err := os.Stat(filename)
	if _, progerr := os.Stat(progressFilename); err == nil && fileinfo.Size() == length && progerr != nil {
		log.Print("File already downloaded")
		return
	}

	progressFile, segmentSize, segmentCount, segments, err := setupProgressFile(progressFilename, length, int64(1000000), err != nil)

	if err != nil {
		log.Fatalf("Failed to setup progress file: %s", err)
	}
	defer progressFile.Close()

	file, err := os.OpenFile(filename, os.O_RDWR|os.O_APPEND|os.O_CREATE, 0666)
	if err != nil {
		log.Fatalf("Unable to open output file: %s", err)
	}
	defer file.Close()

	instructions := make(chan Download)
	updates := make(chan ProgressUpdate)

	for i := uint(0); i < numThreads; i++ {
		go download_segments(i, url, instructions, updates, file, progressFile)
	}

	go print_progress(updates, numThreads)

	for seg := int64(0); seg < segmentCount; seg++ {
		if segments[seg] {
			continue
		}

		left := segmentSize
		pos := seg * segmentSize
		if pos+segmentSize > length {
			left = length - pos
		}

		instructions <- Download{position: pos, size: left, segmentNum: seg}
	}

	for i := uint(0); i < numThreads; i++ {
		instructions <- Download{position: length, size: 0}
	}
}

func main() {
	outputPtr := flag.String("o", "video.mp4", "output file")
	numThreadsPtr := flag.Uint("n", 4, "number of threads")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage of multidown: multidown [-o outputfile] [-n numthreads] url\n")
		flag.PrintDefaults()
	}

	flag.Parse()

	if flag.NArg() < 1 {
		log.Fatal("Need to specify a url to download")
	}

	if *numThreadsPtr == 0 {
		log.Fatal("Running with zero threads means nothing will download")
	}

	download(flag.Arg(0), *outputPtr, *numThreadsPtr)
}
