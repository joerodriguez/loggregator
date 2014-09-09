package websocketserver

import (
	"code.google.com/p/gogoprotobuf/proto"
	"doppler/sinks"
	"doppler/sinks/websocket"
	"doppler/sinkserver/sinkmanager"
	"fmt"
	"github.com/cloudfoundry/dropsonde/events"
	"github.com/cloudfoundry/gosteno"
	"github.com/cloudfoundry/loggregatorlib/server"
	gorilla "github.com/gorilla/websocket"
	"net"
	"net/http"
	"regexp"
	"sync"
	"time"
)

//TODO: change path structure to /apps/[APP_ID]/[ACTION]

const (
	STREAM_LOGS_PATH = "/stream"
	RECENT_LOGS_PATH = "/recent"
	DUMP_LOGS_PATH   = "/dump/"
	FIREHOSE_PATH    = "/firehose"
)

type WebsocketServer struct {
	apiEndpoint       string
	sinkManager       *sinkmanager.SinkManager
	keepAliveInterval time.Duration
	bufferSize        uint
	logger            *gosteno.Logger
	listener          net.Listener
	dropsondeOrigin   string
	sync.RWMutex
}

func New(apiEndpoint string, sinkManager *sinkmanager.SinkManager, keepAliveInterval time.Duration, wSMessageBufferSize uint, logger *gosteno.Logger) *WebsocketServer {
	return &WebsocketServer{
		apiEndpoint:       apiEndpoint,
		sinkManager:       sinkManager,
		keepAliveInterval: keepAliveInterval,
		bufferSize:        wSMessageBufferSize,
		logger:            logger,
		dropsondeOrigin:   sinkManager.DropsondeOrigin,
	}
}

func (w *WebsocketServer) Start() {
	w.logger.Infof("WebsocketServer: Listening for sinks at %s", w.apiEndpoint)

	listener, e := net.Listen("tcp", w.apiEndpoint)
	if e != nil {
		panic(e)
	}

	w.Lock()
	w.listener = listener
	w.Unlock()

	s := &http.Server{Addr: w.apiEndpoint, Handler: w}
	err := s.Serve(w.listener)
	w.logger.Debugf("serve ended with %v", err)
}

func (w *WebsocketServer) Stop() {
	w.Lock()
	defer w.Unlock()
	w.logger.Debug("stopping websocket server")
	w.listener.Close()
}

func (w *WebsocketServer) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	w.logger.Debug("WebsocketServer.ServeHTTP: starting")
	var handler func(string, *gorilla.Conn)

	validPaths := regexp.MustCompile("^/apps/(.*)/(recentlogs|stream)$")
	matches := validPaths.FindStringSubmatch(r.URL.Path)
	if len(matches) != 3 {
		rw.Header().Set("WWW-Authenticate", "Basic")
		rw.WriteHeader(http.StatusNotFound)
		fmt.Fprintf(rw, "Resource Not Found. %s", r.URL.Path)
		return
	}
	appId := matches[1]

	if appId == "" {
		rw.Header().Set("WWW-Authenticate", "Basic")
		rw.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(rw, "App ID missing. Make request to /apps/APP_ID/%s", matches[2])

		w.logger.Debugf("WebsocketServer.ServeHTTP: Validation error (returning 400): Invalid AppId")
		w.logInvalidApp(r.RemoteAddr)
		return
	}
	endpoint := matches[2]

	switch endpoint {
	case "stream":
		handler = w.streamLogs
	case "recentlogs":
		handler = w.recentLogs
	case FIREHOSE_PATH:
		handler = w.streamFirehose
	default:
		w.logger.Debugf("WebsocketServer.ServeHTTP: Invalid path (returning 400): %s", "invalid path "+r.URL.Path)
		http.Error(rw, "invalid path "+r.URL.Path, 400)
		return
	}

	var appId string
	if r.URL.Path != FIREHOSE_PATH {
		var err error
		appId, err = w.validate(r)
		if err != nil {
			w.logger.Debugf("WebsocketServer.ServeHTTP: Validation error (returning 400): %s", err.Error())
			http.Error(rw, err.Error(), 400)
			return
		}
	}

	ws, err := gorilla.Upgrade(rw, r, nil, 1024, 1024)
	if err != nil {
		println("error" + err.Error())
		w.logger.Debugf("WebsocketServer.ServeHTTP: Upgrade error (returning 400): %s", err.Error())
		http.Error(rw, err.Error(), 400)
		return
	}

	defer ws.Close()
	defer ws.WriteControl(gorilla.CloseMessage, gorilla.FormatCloseMessage(gorilla.CloseNormalClosure, ""), time.Time{})

	handler(appId, ws)
}

func (w *WebsocketServer) streamLogs(appId string, ws *gorilla.Conn) {
	w.logger.Debugf("WebsocketServer: Requesting a wss sink for app %s", appId)
	w.streamWebsocket(appId, ws, w.sinkManager.RegisterSink, w.sinkManager.UnregisterSink)
}

func (w *WebsocketServer) streamFirehose(appId string, ws *gorilla.Conn) {
	w.logger.Debugf("WebsocketServer: Requesting firehose wss sink")
	w.streamWebsocket(websocket.FIREHOSE_APP_ID, ws, w.sinkManager.RegisterFirehoseSink, w.sinkManager.UnregisterFirehoseSink)
}

func (w *WebsocketServer) streamWebsocket(appId string, ws *gorilla.Conn, registerFunc func(sinks.Sink) bool, unregisterFunc func(sinks.Sink)) {
	websocketSink := websocket.NewWebsocketSink(
		appId,
		w.logger,
		ws,
		w.bufferSize,
		w.dropsondeOrigin,
	)

	registerFunc(websocketSink)
	defer unregisterFunc(websocketSink)

	go ws.ReadMessage()
	server.NewKeepAlive(ws, w.keepAliveInterval).Run()
}

func (w *WebsocketServer) recentLogs(appId string, ws *gorilla.Conn) {
	logMessages := w.sinkManager.RecentLogsFor(appId)
	sendMessagesToWebsocket(logMessages, ws, w.logger)
}

func (w *WebsocketServer) logInvalidApp(address string) {
	message := fmt.Sprintf("WebsocketServer: Did not accept sink connection with invalid app id: %s.", address)
	w.logger.Warn(message)
}

func sendMessagesToWebsocket(logMessages []*events.Envelope, ws *gorilla.Conn, logger *gosteno.Logger) {
	for _, messageEnvelope := range logMessages {
		envelopeBytes, err := proto.Marshal(messageEnvelope)

		if err != nil {
			logger.Errorf("Websocket Server %s: Error marshalling %s envelope from origin %s: %s", ws.RemoteAddr(), messageEnvelope.GetEventType().String(), messageEnvelope.GetOrigin(), err.Error())
		}

		err = ws.WriteMessage(gorilla.BinaryMessage, envelopeBytes)
		if err != nil {
			logger.Debugf("Websocket Server %s: Error when trying to send data to sink %s. Requesting close. Err: %v", ws.RemoteAddr(), err)
		} else {
			logger.Debugf("Websocket Server %s: Successfully sent data", ws.RemoteAddr())
		}
	}
}
