package main

import (
	"context"
	"encoding/binary"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// Base gtp echo-request packet (without the sequence number counter)
var basePacket = []byte{0x32, 0x01, 0x00, 0x04, 0x00, 0x00}

type response struct {
	rxTime time.Time
	addr   *net.UDPAddr
	data   []byte
}

func main() {

	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)
	log.SetOutput(os.Stdout)

	delay := flag.Int("delay", 1000, "Delay in mili-seconds between sending packets")
	timeout := flag.Int("timeout", 50, "Timeout in mili-seconds for waiting for response")
	target := flag.String("target", "", "Target IP:Port")
	bindAddr := flag.String("bind", "0.0.0.0", "Local IP address to bind to (e.g., 192.168.1.10)")
	bindPort := flag.Int("port", 1234, "Local port to bind to")
	flag.Parse()

	// Parse the bind address
	bindIP := net.ParseIP(*bindAddr)
	if bindIP == nil {
		log.Fatalf("Invalid bind address: %s", *bindAddr)
	}

	// Listen on specified IP and port
	localAddr := &net.UDPAddr{IP: bindIP, Port: *bindPort}
	conn, err := net.ListenUDP("udp4", localAddr)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("Bound to %s", localAddr)

	respChan := make(chan response)

	ctx, cancel := context.WithCancel(context.Background())
	// Handle graceful shutdown on Ctrl+C or Ctrl+D
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigChan
		log.Println("Received shutdown signal...")
		cancel()
	}()

	// Start a goroutine to read and print whatever bytes it receives on the connection
	go func(conn *net.UDPConn, respChan chan response, ctx context.Context) {
		for {
			buffer := make([]byte, 2048)
			n, addr, err := conn.ReadFromUDP(buffer)
			rxTime := time.Now()
			if err != nil {
				log.Fatal(err)
			}
			if ctx.Err() != nil {
				close(respChan)
				conn.Close()
				log.Println("Shutting down read goroutine...", ctx.Err())
				return
			}
			respChan <- response{rxTime: rxTime, addr: addr, data: buffer[:n]}
		}
	}(conn, respChan, ctx)

	log.Println("Starting sending at interval", *delay, "miliseconds and response timeout of", *timeout, "miliseconds")
	counter := uint32(1)
	timeouts := 0
	// Track send times by sequence number for accurate RTT calculation
	sendTimes := make(map[uint32]time.Time)

	for {
		// Sequence number counter is 4 bytes
		counterBytes := make([]byte, 4)
		binary.BigEndian.PutUint32(counterBytes, counter)

		// Create the full packet with the updated counter
		packet := append(basePacket, counterBytes...)
		packet = append(packet, 0x00, 0x00)

		addr, err := net.ResolveUDPAddr("udp", *target)
		if err != nil {
			log.Fatal(err)
		}

		sendTime := time.Now()
		sendTimes[counter] = sendTime
		_, err = conn.WriteTo(packet, addr)
		if err != nil {
			log.Fatal(err)
		}

		log.Printf("%+70s", fmt.Sprintf("Sent GTP echo-request: %x\n", packet))

		// Wait for cancel/sigterm, response or timeout
		select {
		case <-ctx.Done():
			time.Sleep(1 * time.Second)
			fmt.Printf("%+80s", fmt.Sprintf("Total packets sent: %d, Total timeouts: %d, Failure Ratio: %.2f%%\n", counter, timeouts, float64(timeouts)/float64(counter)*100))
			return
		case resp := <-respChan:
			// Extract sequence number from response (bytes 6-9 in GTP packet)
			var respSeq uint32
			var rttStr string
			if len(resp.data) >= 10 {
				respSeq = binary.BigEndian.Uint32(resp.data[6:10])
				if txTime, ok := sendTimes[respSeq]; ok {
					rtt := resp.rxTime.Sub(txTime)
					rttStr = fmt.Sprintf("RTT: %v", rtt)
					delete(sendTimes, respSeq) // Clean up
				} else {
					rttStr = "RTT: unknown (no matching request)"
				}
			} else {
				rttStr = "RTT: unknown (packet too short)"
			}
			log.Printf("ReplyFrom: %+18s Seq: 0x%08x Data: %x %s", resp.addr, respSeq, resp.data, rttStr)
			time.Sleep(time.Duration(*delay) * time.Millisecond)
			counter++
			continue
		case <-time.After(time.Duration(*timeout) * time.Millisecond):
			log.Printf("%-70s", fmt.Sprintf("!!!!!Timeout waiting for response seq number: 0x%x", counter))
			delete(sendTimes, counter) // Clean up timed out entry
			timeouts++
			counter++
		}

	}
}
