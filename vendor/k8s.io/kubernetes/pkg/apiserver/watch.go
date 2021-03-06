/*
Copyright 2014 The Kubernetes Authors All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package apiserver

import (
	"net/http"
	"reflect"
	"regexp"
	"strings"
	"time"

	"k8s.io/kubernetes/pkg/httplog"
	"k8s.io/kubernetes/pkg/runtime"
	"k8s.io/kubernetes/pkg/watch"
	watchjson "k8s.io/kubernetes/pkg/watch/json"

	"github.com/emicklei/go-restful"
	"github.com/golang/glog"
	"golang.org/x/net/websocket"
)

var (
	connectionUpgradeRegex = regexp.MustCompile("(^|.*,\\s*)upgrade($|\\s*,)")

	// nothing will ever be sent down this channel
	neverExitWatch <-chan time.Time = make(chan time.Time)
)

func isWebsocketRequest(req *http.Request) bool {
	return connectionUpgradeRegex.MatchString(strings.ToLower(req.Header.Get("Connection"))) && strings.ToLower(req.Header.Get("Upgrade")) == "websocket"
}

// timeoutFactory abstracts watch timeout logic for testing
type timeoutFactory interface {
	TimeoutCh() (<-chan time.Time, func() bool)
}

// realTimeoutFactory implements timeoutFactory
type realTimeoutFactory struct {
	timeout time.Duration
}

// TimeoutChan returns a channel which will receive something when the watch times out,
// and a cleanup function to call when this happens.
func (w *realTimeoutFactory) TimeoutCh() (<-chan time.Time, func() bool) {
	if w.timeout == 0 {
		return neverExitWatch, func() bool { return false }
	}
	t := time.NewTimer(w.timeout)
	return t.C, t.Stop
}

// serveWatch handles serving requests to the server
func serveWatch(watcher watch.Interface, scope RequestScope, req *restful.Request, res *restful.Response, timeout time.Duration) {
	s, mediaType, err := negotiateOutputSerializer(req.Request, scope.Serializer)
	if err != nil {
		scope.err(err, res.ResponseWriter, req.Request)
		return
	}
	// TODO: replace with typed serialization
	if mediaType != "application/json" {
		writeRawJSON(http.StatusNotAcceptable, (errNotAcceptable{[]string{"application/json"}}).Status(), res.ResponseWriter)
		return
	}
	encoder := scope.Serializer.EncoderForVersion(s, scope.Kind.GroupVersion())
	watchServer := &WatchServer{watcher, encoder, func(obj runtime.Object) {
		if err := setSelfLink(obj, req, scope.Namer); err != nil {
			glog.V(5).Infof("Failed to set self link for object %v: %v", reflect.TypeOf(obj), err)
		}
	}, &realTimeoutFactory{timeout}}
	if isWebsocketRequest(req.Request) {
		websocket.Handler(watchServer.HandleWS).ServeHTTP(httplog.Unlogged(res.ResponseWriter), req.Request)
	} else {
		watchServer.ServeHTTP(res.ResponseWriter, req.Request)
	}
}

// WatchServer serves a watch.Interface over a websocket or vanilla HTTP.
type WatchServer struct {
	watching watch.Interface
	encoder  runtime.Encoder
	fixup    func(runtime.Object)
	t        timeoutFactory
}

// HandleWS implements a websocket handler.
func (w *WatchServer) HandleWS(ws *websocket.Conn) {
	defer ws.Close()
	done := make(chan struct{})
	go func() {
		var unused interface{}
		// Expect this to block until the connection is closed. Client should not
		// send anything.
		websocket.JSON.Receive(ws, &unused)
		close(done)
	}()
	for {
		select {
		case <-done:
			w.watching.Stop()
			return
		case event, ok := <-w.watching.ResultChan():
			if !ok {
				// End of results.
				return
			}
			w.fixup(event.Object)
			obj, err := watchjson.Object(w.encoder, &event)
			if err != nil {
				// Client disconnect.
				w.watching.Stop()
				return
			}
			if err := websocket.JSON.Send(ws, obj); err != nil {
				// Client disconnect.
				w.watching.Stop()
				return
			}
		}
	}
}

// ServeHTTP serves a series of JSON encoded events via straight HTTP with
// Transfer-Encoding: chunked.
func (self *WatchServer) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	loggedW := httplog.LogOf(req, w)
	w = httplog.Unlogged(w)
	timeoutCh, cleanup := self.t.TimeoutCh()
	defer cleanup()
	defer self.watching.Stop()

	cn, ok := w.(http.CloseNotifier)
	if !ok {
		loggedW.Addf("unable to get CloseNotifier: %#v", w)
		http.NotFound(w, req)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		loggedW.Addf("unable to get Flusher: %#v", w)
		http.NotFound(w, req)
		return
	}
	w.Header().Set("Transfer-Encoding", "chunked")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	// TODO: use arbitrary serialization on watch
	encoder := watchjson.NewEncoder(w, self.encoder)
	for {
		select {
		case <-cn.CloseNotify():
			return
		case <-timeoutCh:
			return
		case event, ok := <-self.watching.ResultChan():
			if !ok {
				// End of results.
				return
			}
			self.fixup(event.Object)
			if err := encoder.Encode(&event); err != nil {
				// Client disconnect.
				return
			}
			flusher.Flush()
		}
	}
}
