package main

import (
	"context"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"time"
)

var (
	httpAddr = ":5624"
	bitrate  = 2000000
	fps      = 24
	width    = 1280
	height   = 720
)

func init() {
	flag.StringVar(&httpAddr, "http", httpAddr, "HTTP server address")
	flag.IntVar(&bitrate, "bitrate", bitrate, "H264 bitrate")
	flag.IntVar(&fps, "fps", fps, "Frames per second")
	flag.IntVar(&width, "width", width, "Width of frame")
	flag.IntVar(&height, "height", height, "Height of frame")
	flag.Parse()
}

func main() {
	wg := new(sync.WaitGroup)
	ctx, cancel := context.WithCancel(context.Background())
	signalCh := make(chan os.Signal, 1)
	go func() {
		log.Println(<-signalCh)
		cancel()
	}()
	signal.Notify(signalCh, os.Interrupt)

	mp4, err := record(ctx, cancel, wg)
	if err != nil {
		cancel()
		log.Println(err)
		wg.Wait()
		return
	}

	ch := make(chan []byte)
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer mp4.Close()
		err := parse(ctx, mp4, ch)
		if err != nil {
			cancel()
			log.Println(err)
		}
	}()

	select {
	case first := <-ch:
		httpServer(ctx, cancel, first, buffer(ch, 4))
	case <-ctx.Done():
	}
	wg.Wait()
}

func record(ctx context.Context, cancel context.CancelFunc, wg *sync.WaitGroup) (io.ReadCloser, error) {
	cameraCmd, ffmpegCmd := commands()
	cameraArgs := strings.Split(cameraCmd, " ")
	ffmpegArgs := strings.Split(ffmpegCmd, " ")
	camera := exec.CommandContext(ctx, cameraArgs[0], cameraArgs[1:]...)
	ffmpeg := exec.CommandContext(ctx, ffmpegArgs[0], ffmpegArgs[1:]...)
	camera.Stderr = os.Stderr
	ffmpeg.Stderr = os.Stderr

	h264Stream, err := camera.StdoutPipe()
	if err != nil {
		return nil, err
	}
	ffmpeg.Stdin = h264Stream

	mp4, err := ffmpeg.StdoutPipe()
	if err != nil {
		return nil, err
	}
	err = startProcess(camera, cancel, wg)
	if err != nil {
		return nil, err
	}
	err = startProcess(ffmpeg, cancel, wg)
	if err != nil {
		return nil, err
	}
	h264Stream.Close()
	return mp4, nil
}

func commands() (string, string) {
	camera := "/opt/vc/bin/raspivid -b %[1]d -g %[2]d -t 0 -fps %[2]d -ih -pf main -lev 4 -ih -fl -w %[3]d -h %[4]d -n -o -"
	ffmpeg := "ffmpeg -f h264 -r %d -i - -c:v copy -f mp4 -movflags frag_keyframe+empty_moov+default_base_moof -"
	camera = fmt.Sprintf(camera, bitrate, fps, width, height)
	ffmpeg = fmt.Sprintf(ffmpeg, fps)
	log.Println(camera)
	log.Println(ffmpeg)
	return camera, ffmpeg
}

func startProcess(cmd *exec.Cmd, cancel context.CancelFunc, wg *sync.WaitGroup) error {
	err := cmd.Start()
	if err != nil {
		return err
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		err := cmd.Wait()
		if err != nil {
			cancel()
			log.Println(err)
		}
	}()
	return nil
}

func parse(ctx context.Context, r io.Reader, ch chan []byte) error {
	bufSize := bitrate / 8 * 2
	buf := make([]byte, bufSize)
	position := 0
	lastType := ""
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		switch lastType {
		case "mdat", "moov":
			ch <- buf[:position]
			buf = make([]byte, bufSize)
			position = 0
		}
		_, err := io.ReadFull(r, buf[position:position+4])
		if err != nil {
			return err
		}
		length := int(binary.BigEndian.Uint32(buf[position : position+4]))
		_, err = io.ReadFull(r, buf[position+4:position+length])
		if err != nil {
			return err
		}
		lastType = string(buf[position+4 : position+8])
		position = position + length
	}
}

func buffer(in chan []byte, capacity int) chan []byte {
	out := make(chan []byte)
	items := make([][]byte, 0, capacity)
	go func() {
		for {
			if len(items) == 0 {
				items = append(items, <-in)
			}
			select {
			case newItem := <-in:
				if len(items) == capacity {
					n := copy(items, items[1:])
					items = items[:n]
				}
				items = append(items, newItem)
			case out <- items[0]:
				n := copy(items, items[1:])
				items = items[:n]
			}
		}
	}()
	return out
}

func httpServer(ctx context.Context, cancel context.CancelFunc, first []byte, stream chan []byte) {
	mutex := make(chan int, 1)
	http.HandleFunc("/v", func(w http.ResponseWriter, req *http.Request) {
		select {
		case mutex <- 1:
			defer func() {
				<-mutex
			}()
			break
		case <-time.After(time.Second * 5):
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		_, err := w.Write(first)
		if err != nil {
			log.Println(err)
			return
		}
		for {
			select {
			case <-ctx.Done():
				return
			case buf := <-stream:
				_, err = w.Write(buf)
				if err != nil {
					log.Println(err)
					return
				}
			}
		}
	})
	server := &http.Server{Addr: httpAddr}
	go func() {
		err := server.ListenAndServe()
		if err != nil && err != http.ErrServerClosed {
			cancel()
			log.Println(err)
		}
	}()
	<-ctx.Done()
	err := server.Shutdown(context.Background())
	if err != nil {
		log.Println(err)
	}
}
