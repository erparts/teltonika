// Copyright 2022 Alim Zanibekov
//
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file or at
// https://opensource.org/licenses/MIT.

package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"

	"github.com/alim-zanibekov/teltonika"
	"github.com/at-wat/mqtt-go"
)

var decodeConfig = &teltonika.DecodeConfig{IoElementsAlloc: teltonika.OnReadBuffer}

type Logger struct {
	Info  *log.Logger
	Error *log.Logger
}

type TrackersHub interface {
	SendPacket(imei string, packet *teltonika.Packet) error
	ListClients() []*TCPClient
}

type TCPServer struct {
	address   string
	clients   *sync.Map
	logger    *Logger
	OnPacket  func(imei string, pkt *teltonika.Packet)
	OnClose   func(imei string)
	OnConnect func(imei string)
}

type TCPClient struct {
	conn net.Conn
	imei string
}

//goland:noinspection GoUnusedExportedFunction
func NewTCPServer(address string) *TCPServer {
	return &TCPServer{address: address, logger: &Logger{log.Default(), log.Default()}, clients: &sync.Map{}}
}

func NewTCPServerLogger(address string, logger *Logger) *TCPServer {
	return &TCPServer{address: address, logger: logger, clients: &sync.Map{}}
}

func (r *TCPServer) Run() error {
	logger := r.logger

	addr, err := net.ResolveTCPAddr("tcp", r.address)
	if err != nil {
		return fmt.Errorf("tcp address resolve error (%v)", err)
	}

	listener, err := net.ListenTCP("tcp", addr)
	if err != nil {
		return fmt.Errorf("tcp listener create error (%v)", err)
	}

	defer func() {
		_ = listener.Close()
	}()

	logger.Info.Println("tcp server listening at " + r.address)

	for {
		conn, err := listener.Accept()
		if err != nil {
			return fmt.Errorf("tcp connection accept error (%v)", err)
		}
		go r.handleConnection(conn)
	}
}

func (r *TCPServer) SendPacket(imei string, packet *teltonika.Packet) error {
	clientRaw, ok := r.clients.Load(imei)
	if !ok {
		return fmt.Errorf("client with imei '%s' not found", imei)
	}
	client := clientRaw.(*TCPClient)

	buf, err := teltonika.EncodePacket(packet)
	if err != nil {
		return err
	}

	if _, err = client.conn.Write(buf); err != nil {
		return err
	}

	return nil
}

func (r *TCPServer) ListClients() []*TCPClient {
	clients := make([]*TCPClient, 0, 10)
	r.clients.Range(func(key, value interface{}) bool {
		clients = append(clients, value.(*TCPClient))
		return true
	})
	return clients
}

func (r *TCPServer) handleConnection(conn net.Conn) {
	logger := r.logger
	client := &TCPClient{conn, ""}
	imei := ""

	addr := conn.RemoteAddr().String()

	defer func() {
		if r.OnClose != nil && imei != "" {
			r.OnClose(imei)
		}
		if imei != "" {
			logger.Info.Printf("[%s]: disconnected", imei)
			r.clients.Delete(imei)
		} else {
			logger.Info.Printf("[%s]: disconnected", addr)
		}

		if err := conn.Close(); err != nil {
			logger.Error.Printf("[%s]: connection close error (%v)", addr, err)
		}
	}()

	logger.Info.Printf("[%s]: connected", addr)

	buf := make([]byte, 1024)
	size, err := conn.Read(buf)
	if err != nil {
		logger.Error.Printf("[%s]: connection read error (%v)", addr, err)
		return
	}
	if size < 2 {
		logger.Error.Printf("[%s]: invalid first message (read: %s)", addr, hex.EncodeToString(buf))
		return
	}
	imeiLen := int(binary.BigEndian.Uint16(buf[:2]))
	buf = buf[2:]

	if len(buf) < imeiLen {
		logger.Error.Printf("[%s]: invalid imei size (read: %s)", addr, hex.EncodeToString(buf))
		return
	}

	imei = strings.TrimSpace(string(buf[:imeiLen]))

	if imei == "" {
		logger.Error.Printf("[%s]: invalid imei '%s'", addr, imei)
		return
	}

	client.imei = imei

	if r.OnConnect != nil {
		r.OnConnect(imei)
	}

	r.clients.Store(imei, client)

	logger.Info.Printf("[%s]: imei - %s", addr, client.imei)

	if _, err = conn.Write([]byte{1}); err != nil {
		logger.Error.Printf("[%s]: error writing ack (%v)", client.imei, err)
		return
	}

	readBuffer := make([]byte, 1300)
	for {
		if err = conn.SetReadDeadline(time.Now().Add(time.Minute * 15)); err != nil {
			logger.Error.Printf("[%s]: SetReadDeadline error (%v)", imei, err)
			return
		}
		read, res, err := teltonika.DecodeTCPFromReaderBuf(conn, readBuffer, decodeConfig)
		if err != nil {
			logger.Error.Printf("[%s]: packet decode error (%v)", imei, err)
			return
		}

		if res.Response != nil {
			if _, err = conn.Write(res.Response); err != nil {
				logger.Error.Printf("[%s]: error writing response (%v)", imei, err)
				return
			}
		}

		logger.Info.Printf("[%s]: message: %s", imei, hex.EncodeToString(readBuffer[:read]))
		jsonData, err := json.Marshal(res.Packet)
		if err != nil {
			logger.Error.Printf("[%s]: decoder result marshaling error (%v)", imei, err)
		}
		logger.Info.Printf("[%s]: decoded: %s", imei, string(jsonData))

		if r.OnPacket != nil {
			r.OnPacket(imei, res.Packet)
		}
	}
}

type HTTPServer struct {
	address  string
	hub      TrackersHub
	respChan *sync.Map
	logger   *Logger
}

//goland:noinspection GoUnusedExportedFunction
func NewHTTPServer(address string, hub TrackersHub) *HTTPServer {
	return &HTTPServer{address: address, respChan: &sync.Map{}, hub: hub}
}

func NewHTTPServerLogger(address string, hub TrackersHub, logger *Logger) *HTTPServer {
	return &HTTPServer{address: address, respChan: &sync.Map{}, hub: hub, logger: logger}
}

func (hs *HTTPServer) Run() error {
	logger := hs.logger

	handler := http.NewServeMux()

	handler.HandleFunc("/cmd", hs.handleCmd)

	handler.HandleFunc("/list-clients", hs.listClients)

	logger.Info.Println("http server listening at " + hs.address)

	err := http.ListenAndServe(hs.address, handler)
	if err != nil {
		return fmt.Errorf("http listen error (%v)", err)
	}
	return nil
}

func (hs *HTTPServer) WriteMessage(imei string, message *teltonika.Message) {
	ch, ok := hs.respChan.Load(imei)
	if ok {
		select {
		case ch.(chan *teltonika.Message) <- message:
		}
	}
}

func (hs *HTTPServer) ClientDisconnected(imei string) {
	ch, ok := hs.respChan.Load(imei)
	if ok {
		select {
		case ch.(chan *teltonika.Message) <- nil:
		}
	}
}

func (hs *HTTPServer) listClients(w http.ResponseWriter, _ *http.Request) {
	for _, client := range hs.hub.ListClients() {
		_, err := w.Write([]byte(client.conn.RemoteAddr().String() + " - " + client.imei + "\n"))
		if err != nil {
			return
		}
	}
}

func (hs *HTTPServer) handleCmd(w http.ResponseWriter, r *http.Request) {
	logger := hs.logger

	params := r.URL.Query()
	imei := params.Get("imei")
	buf := make([]byte, 512)
	n, _ := r.Body.Read(buf)
	cmd := string(buf[:n])

	packet := &teltonika.Packet{
		CodecID:  teltonika.Codec12,
		Data:     nil,
		Messages: []teltonika.Message{{Type: teltonika.TypeCommand, Text: strings.TrimSpace(cmd)}},
	}

	result := make(chan *teltonika.Message, 1)
	defer close(result)
	for {
		if _, loaded := hs.respChan.LoadOrStore(imei, result); !loaded {
			break
		}
		time.Sleep(time.Millisecond * 100)
	}

	defer hs.respChan.Delete(imei)

	if err := hs.hub.SendPacket(imei, packet); err != nil {
		logger.Error.Printf("send packet error (%v)", err)
		w.WriteHeader(http.StatusBadRequest)
		_, err = w.Write([]byte(err.Error() + "\n"))
		if err != nil {
			logger.Error.Printf("http write error (%v)", err)
		}
	} else {
		logger.Info.Printf("command '%s' sent to '%s'", cmd, imei)
		ticker := time.NewTimer(time.Minute * 3)
		defer ticker.Stop()

		select {
		case msg := <-result:
			if msg != nil {
				_, err = w.Write([]byte(msg.Text + "\n"))
			} else {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, err = w.Write([]byte("tracker disconnected\n"))
			}
		case <-ticker.C:
			w.WriteHeader(http.StatusGatewayTimeout)
			_, err = w.Write([]byte("tracker response timeout exceeded\n"))
		}

		if err != nil {
			logger.Error.Printf("http write error (%v)", err)
		}
	}
}

func NewMQTTClient(url string) (mqtt.Client, error) {
	return mqtt.NewReconnectClient(
		&mqtt.URLDialer{
			URL: fmt.Sprintf("mqtt://%s", url),
			Options: []mqtt.DialOption{
				mqtt.WithConnStateHandler(func(s mqtt.ConnState, err error) {
					switch s {
					case mqtt.StateActive:
						logger.Info.Printf("MQTT connected to %q", url)
					case mqtt.StateDisconnected, mqtt.StateClosed:
						logger.Info.Printf("MQTT discconected from %q (err: %v)", url, err)
					default:
						logger.Info.Printf("MQTT state change to %s (err: %v)", s, err)
					}
				}),
			},
		},
		mqtt.WithPingInterval(10*time.Second),
		mqtt.WithTimeout(5*time.Second),
		mqtt.WithReconnectWait(1*time.Second, 15*time.Second),
		mqtt.WithRetryClient(&mqtt.RetryClient{
			DirectlyPublishQoS0: false,
			ResponseTimeout:     time.Second * 10,
		}),
	)
}

var logger *Logger

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	var httpAddress string
	var tcpAddress string
	var broker string
	var clientID string
	var publishTopic string
	flag.StringVar(&tcpAddress, "address", "0.0.0.0:8080", "tcp server address")
	flag.StringVar(&httpAddress, "http", "0.0.0.0:8081", "http server address")
	flag.StringVar(&broker, "broker", "localhost:1883", "mqtt broker")
	flag.StringVar(&publishTopic, "topic", "tcp-server", "mqtt topic")
	flag.StringVar(&clientID, "client-id", "clien-42", "mqtt client ID")
	flag.Parse()

	logger = &Logger{
		Info:  log.New(os.Stdout, "INFO: ", log.Ldate|log.Ltime),
		Error: log.New(os.Stdout, "ERROR: ", log.Ldate|log.Ltime|log.Lshortfile),
	}

	m, err := NewMQTTClient(broker)
	if err != nil {
		panic(err)
	}

	if _, err := m.Connect(ctx, clientID, mqtt.WithKeepAlive(30)); err != nil {
		panic(err)
	}

	serverTcp := NewTCPServerLogger(tcpAddress, logger)
	serverHttp := NewHTTPServerLogger(httpAddress, serverTcp, logger)

	serverTcp.OnPacket = func(imei string, pkt *teltonika.Packet) {
		payload := map[string]any{
			"imei":   imei,
			"packet": pkt,
			"time":   time.Now(),
		}

		if pkt.Messages != nil && len(pkt.Messages) > 0 {
			serverHttp.WriteMessage(imei, &pkt.Messages[0])
		}
		if pkt.Data != nil {
			go func() {
				// MQTT publish
				payloadJSON, _ := json.Marshal(payload)
				m.Publish(ctx, &mqtt.Message{
					Topic:   publishTopic,
					QoS:     mqtt.QoS0,
					Payload: payloadJSON,
				})
			}()
		}
	}

	serverTcp.OnClose = func(imei string) {
		serverHttp.ClientDisconnected(imei)
	}

	go func() {
		panic(serverTcp.Run())
	}()
	panic(serverHttp.Run())
}

func buildJsonPacket(imei string, pkt *teltonika.Packet) []byte {
	if pkt.Data == nil {
		return nil
	}
	gpsFrames := make([]interface{}, 0)
	for _, frame := range pkt.Data {
		gpsFrames = append(gpsFrames, map[string]interface{}{
			"timestamp": int64(frame.TimestampMs / 1000.0),
			"lat":       frame.Lat,
			"lon":       frame.Lng,
		})
	}
	if len(gpsFrames) == 0 {
		return nil
	}
	values := map[string]interface{}{
		"deveui": imei,
		"time":   time.Now().String(),
		"frames": map[string]interface{}{
			"gps": gpsFrames,
		},
	}
	jsonValue, _ := json.Marshal(values)
	return jsonValue
}

func hookSend(outHook string, imei string, pkt *teltonika.Packet, logger *Logger) {
	jsonValue := buildJsonPacket(imei, pkt)
	if jsonValue == nil {
		return
	}
	res, err := http.Post(outHook, "application/json", bytes.NewBuffer(jsonValue))
	if err != nil {
		logger.Error.Printf("http post error (%v)", err)
	} else {
		logger.Info.Printf("packet sent to output hook, status: %s", res.Status)
	}
}
