package main

import (
	"bytes"
	"fmt"
	"golang.org/x/sync/errgroup"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

var (
	sps0, pps0, sps1, pps1 []byte
	readyChan              = make(chan bool)
	clients                = make(map[*websocket.Conn]bool) // connected clients
	mutex                  sync.Mutex
	lossRate               int32
)

func init() {
	rand.Seed(time.Now().UnixNano())
}

// Returns true x percent of the time.
func chance(x int32) bool {
	if x < 0 || x > 100 {
		return false
	}
	return rand.Float64() < float64(x)/100.0
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

func startFFmpeg() error {
	var cmd *exec.Cmd
	cmd = exec.Command("ffmpeg", "-f", "v4l2", "-framerate", "30", "-video_size", "640x480", "-i", "/dev/video0", "-pix_fmt", "yuv420p", "-c:v", "libx264", "-preset", "ultrafast", "-tune", "zerolatency", "-profile:v", "baseline", "-f", "h264", "pipe:1")
	cmd.Stderr = os.Stderr

	pipe, err := cmd.StdoutPipe()
	if err != nil {
		log.Println("Error creating StdoutPipe for Cmd", err)
		return err
	}

	err = cmd.Start()
	if err != nil {
		log.Println("Error starting cmd", err)
		return err
	}

	buffer := make([]byte, 1024*1024)
	var dataBuffer bytes.Buffer
	nalCount := 0
	nalDropped := 0
	nalSent := 0
	for {
		n, err := pipe.Read(buffer)
		if err != nil {
			break
		}
		dataBuffer.Write(buffer[:n])

		for {
			nal, rest := findNAL(dataBuffer.Bytes())
			if nal == nil {
				break
			}

			nalCount++

			switch nalCount {
			case 0:
				sps0 = nal
			case 1:
				pps0 = nal
			case 2:
				sps1 = nal
			case 3:
				pps1 = nal
				readyChan <- true
			default:
				lr := atomic.LoadInt32(&lossRate)
				if chance(lr) {
					nalDropped++
				} else {
					nalSent++
					for client := range clients {
						_ = client.WriteMessage(websocket.BinaryMessage, nal)
					}
				}
			}

			if nalCount%100 == 0 {
				fmt.Printf("*** Nal sent: %d, Nal dropped %d **** \n", nalSent, nalDropped)
			}
			dataBuffer = *bytes.NewBuffer(rest)
		}
	}
	return cmd.Wait()
}

func handleConnection(w http.ResponseWriter, r *http.Request) {
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println(err)
		return
	}
	defer func() {
		mutex.Lock()
		delete(clients, ws)
		mutex.Unlock()
		ws.Close()
	}()

	for {
		_, p, err := ws.ReadMessage()
		if err != nil {
			log.Println(err)
			return
		}

		if string(p) == "stream" {
			// Send stored sps and pps
			ws.WriteMessage(websocket.BinaryMessage, sps0)
			ws.WriteMessage(websocket.BinaryMessage, pps0)
			ws.WriteMessage(websocket.BinaryMessage, sps1)
			ws.WriteMessage(websocket.BinaryMessage, pps1)

			mutex.Lock()
			clients[ws] = true
			mutex.Unlock()
		} else if string(p) == "stop" {
			mutex.Lock()
			delete(clients, ws)
			mutex.Unlock()
		}
	}
}

func findNAL(b []byte) ([]byte, []byte) {
	start := bytes.Index(b, []byte{0x00, 0x00, 0x00, 0x01})
	if start == -1 {
		return nil, b
	}
	end := bytes.Index(b[start+4:], []byte{0x00, 0x00, 0x00, 0x01})
	if end == -1 {
		return b[start:], nil
	}
	return b[start : start+4+end], b[start+4+end:]
}

func setLossRateHandler(w http.ResponseWriter, r *http.Request) {
	// Extracting the lossRate from the request query parameters
	newLossRateStr := r.URL.Query().Get("lossRate")
	if newLossRateStr == "" {
		http.Error(w, "lossRate not provided", http.StatusBadRequest)
		return
	}

	// Convert the string to float
	newLossRate, err := strconv.ParseInt(newLossRateStr, 10, 32)
	if err != nil {
		http.Error(w, "Invalid lossRate value", http.StatusBadRequest)
		return
	}

	// Check if the lossRate is between 0 and 100
	if newLossRate < 0 || newLossRate > 100 {
		http.Error(w, "lossRate should be between 0 and 100", http.StatusBadRequest)
		return
	}

	// Safely set the global lossRate
	atomic.SwapInt32(&lossRate, int32(newLossRate))
	fmt.Fprintf(w, "Loss rate set to %d", lossRate)
}

func main() {
	eg := errgroup.Group{}

	eg.Go(startFFmpeg)
	eg.Go(func() error {
		<-readyChan
		fmt.Println("Starting server")

		// WebSocket handler
		http.HandleFunc("/ws", handleConnection)

		http.HandleFunc("/setLossRate", setLossRateHandler)

		// Serve static files from the ./static/ directory when accessing root
		http.Handle("/", http.FileServer(http.Dir("./static/")))

		return http.ListenAndServe(":8080", nil)
	})
	err := eg.Wait()
	if err != nil {
		log.Fatal(err)
	}
}
