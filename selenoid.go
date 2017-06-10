package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/aerokube/selenoid/session"
	"github.com/docker/docker/api/types"
	"golang.org/x/net/websocket"
)

var (
	httpClient *http.Client = &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	num     uint64
	numLock sync.Mutex
)

type request struct {
	*http.Request
}

type sess struct {
	addr string
	id   string
}

// TODO There is simpler way to do this
func (r request) localaddr() string {
	addr := r.Context().Value(http.LocalAddrContextKey).(net.Addr).String()
	_, port, _ := net.SplitHostPort(addr)
	return net.JoinHostPort("127.0.0.1", port)
}

func (r request) session(id string) *sess {
	return &sess{r.localaddr(), id}
}

func (s *sess) url() string {
	return fmt.Sprintf("http://%s/wd/hub/session/%s", s.addr, s.id)
}

type caps struct {
	Name             string `json:"browserName"`
	Version          string `json:"version"`
	ScreenResolution string `json:"screenResolution"`
	VNC              bool   `json:"enableVNC"`
}

func jsonError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(
		map[string]interface{}{
			"value": map[string]string{
				"message": msg,
			},
			"status": 13,
		})
}

func (s *sess) Delete() {
	log.Printf("[SESSION_TIMED_OUT] [%s]\n", s.id)
	r, err := http.NewRequest(http.MethodDelete, s.url(), nil)
	if err != nil {
		log.Printf("[DELETE_FAILED] [%s] [%v]\n", s.id, err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), sessionDeleteTimeout)
	defer cancel()
	resp, err := httpClient.Do(r.WithContext(ctx))
	if resp != nil {
		defer resp.Body.Close()
	}
	if err == nil && resp.StatusCode == http.StatusOK {
		return
	}
	if err != nil {
		log.Printf("[DELETE_FAILED] [%s] [%v]\n", s.id, err)
	} else {
		log.Printf("[DELETE_FAILED] [%s] [%s]\n", s.id, resp.Status)
	}
}

func serial() uint64 {
	numLock.Lock()
	defer numLock.Unlock()
	id := num
	num++
	return id
}

func create(w http.ResponseWriter, r *http.Request) {
	sessionStartTime := time.Now()
	id := serial()
	quota, _, ok := r.BasicAuth()
	if !ok {
		quota = "unknown"
	}
	body, err := ioutil.ReadAll(r.Body)
	r.Body.Close()
	if err != nil {
		log.Printf("[%d] [ERROR_READING_REQUEST] [%s] [%v]\n", id, quota, err)
		jsonError(w, err.Error(), http.StatusBadRequest)
		queue.Drop()
		return
	}
	var browser struct {
		Caps caps `json:"desiredCapabilities"`
	}
	err = json.Unmarshal(body, &browser)
	if err != nil {
		log.Printf("[%d] [BAD_JSON_FORMAT] [%s] [%v]\n", id, quota, err)
		jsonError(w, err.Error(), http.StatusBadRequest)
		queue.Drop()
		return
	}
	resolution, err := getScreenResolution(browser.Caps.ScreenResolution)
	if err != nil {
		log.Printf("[%d] [BAD_SCREEN_RESOLUTION] [%s] [%s]\n", id, quota, browser.Caps.ScreenResolution)
		jsonError(w, err.Error(), http.StatusBadRequest)
		queue.Drop()
		return
	}
	browser.Caps.ScreenResolution = resolution
	starter, ok := manager.Find(browser.Caps.Name, &browser.Caps.Version, browser.Caps.ScreenResolution, browser.Caps.VNC, id)
	if !ok {
		log.Printf("[%d] [ENVIRONMENT_NOT_AVAILABLE] [%s] [%s-%s]\n", id, quota, browser.Caps.Name, browser.Caps.Version)
		jsonError(w, "Requested environment is not available", http.StatusBadRequest)
		queue.Drop()
		return
	}
	u, container, vnc, cancel, err := starter.StartWithCancel()
	if err != nil {
		log.Printf("[%d] [SERVICE_STARTUP_FAILED] [%s] [%v]\n", id, quota, err)
		jsonError(w, err.Error(), http.StatusInternalServerError)
		queue.Drop()
		return
	}
	var resp *http.Response
	i := 1
	for ; ; i++ {
		r.URL.Host, r.URL.Path = u.Host, path.Clean(u.Path+r.URL.Path)
		req, _ := http.NewRequest(http.MethodPost, r.URL.String(), bytes.NewReader(body))
		ctx, done := context.WithTimeout(r.Context(), newSessionAttemptTimeout)
		defer done()
		log.Printf("[%d] [SESSION_ATTEMPTED] [%s] [%s] [%d]\n", id, quota, u.String(), i)
		rsp, err := httpClient.Do(req.WithContext(ctx))
		select {
		case <-ctx.Done():
			if rsp != nil {
				rsp.Body.Close()
			}
			switch ctx.Err() {
			case context.DeadlineExceeded:
				log.Printf("[%d] [SESSION_ATTEMPT_TIMED_OUT] [%s]\n", id, quota)
				continue
			case context.Canceled:
				log.Printf("[%d] [CLIENT_DISCONNECTED] [%s]\n", id, quota)
				queue.Drop()
				cancel()
				return
			}
		default:
		}
		if err != nil {
			if rsp != nil {
				rsp.Body.Close()
			}
			log.Printf("[%d] [SESSION_FAILED] [%s] - [%s]\n", id, u.String(), err)
			jsonError(w, err.Error(), http.StatusInternalServerError)
			queue.Drop()
			cancel()
			return
		}
		if rsp.StatusCode == http.StatusNotFound && u.Path == "" {
			u.Path = "/wd/hub"
			continue
		}
		resp = rsp
		break
	}
	defer resp.Body.Close()
	var s struct {
		Value struct {
			ID string `json:"sessionId"`
		}
		ID string `json:"sessionId"`
	}
	location := resp.Header.Get("Location")
	if location != "" {
		l, err := url.Parse(location)
		if err == nil {
			fragments := strings.Split(l.Path, "/")
			s.ID = fragments[len(fragments)-1]
			u := &url.URL{
				Scheme: "http",
				Host:   hostname,
				Path:   path.Join("/wd/hub/session", s.ID),
			}
			w.Header().Add("Location", u.String())
			w.WriteHeader(resp.StatusCode)
		}
	} else {
		tee := io.TeeReader(resp.Body, w)
		w.WriteHeader(resp.StatusCode)
		json.NewDecoder(tee).Decode(&s)
		if s.ID == "" {
			s.ID = s.Value.ID
		}
	}
	if s.ID == "" {
		log.Printf("[%d] [SESSION_FAILED] [%s] [Bad response from %s - %v]\n", id, quota, u.String(), resp.Status)
		jsonError(w, "protocol error: could not determine session id", http.StatusBadGateway)
		queue.Drop()
		cancel()
		return
	}
	sessions.Put(s.ID, &session.Session{
		Quota:     quota,
		Browser:   browser.Caps.Name,
		Version:   browser.Caps.Version,
		URL:       u,
		Container: container,
		VNC:       vnc,
		Screen:    browser.Caps.ScreenResolution,
		Cancel:    cancel,
		Timeout: onTimeout(timeout, func() {
			request{r}.session(s.ID).Delete()
		})})
	queue.Create()
	log.Printf("[%d] [SESSION_CREATED] [%s] [%s] [%s] [%d] [%v]\n", id, quota, s.ID, u, i, time.Since(sessionStartTime))
}

func getScreenResolution(input string) (string, error) {
	if input == "" {
		return "1920x1080x24", nil
	}
	fullFormat := regexp.MustCompile(`^[0-9]+x[0-9]+x(8|16|24)$`)
	shortFormat := regexp.MustCompile(`^[0-9]+x[0-9]+$`)
	if fullFormat.MatchString(input) {
		return input, nil
	}
	if shortFormat.MatchString(input) {
		return fmt.Sprintf("%sx24", input), nil
	}
	return "", fmt.Errorf(
		"Malformed screenResolution capability: %s. Correct format is WxH (1920x1080) or WxHxD (1920x1080x24).",
		input,
	)
}

func proxy(w http.ResponseWriter, r *http.Request) {
	done := make(chan func())
	go func(w http.ResponseWriter, r *http.Request) {
		cancel := func() {}
		defer func() {
			done <- cancel
		}()
		(&httputil.ReverseProxy{
			Director: func(r *http.Request) {
				fragments := strings.Split(r.URL.Path, "/")
				id := fragments[2]
				sess, ok := sessions.Get(id)
				if ok {
					sess.Lock.Lock()
					defer sess.Lock.Unlock()
					r.URL.Host, r.URL.Path = sess.URL.Host, path.Clean(sess.URL.Path+r.URL.Path)
					close(sess.Timeout)
					if r.Method == http.MethodDelete && len(fragments) == 3 {
						cancel = sess.Cancel
						sessions.Remove(id)
						queue.Release()
						log.Printf("[SESSION_DELETED] [%s]\n", id)
					} else {
						sess.Timeout = onTimeout(timeout, func() {
							request{r}.session(id).Delete()
						})
					}
					return
				}
				r.URL.Path = "/error"
			},
		}).ServeHTTP(w, r)
	}(w, r)
	go (<-done)()
}

func vnc(wsconn *websocket.Conn) {
	defer wsconn.Close()
	sid := strings.Split(wsconn.Request().URL.Path, "/")[2]
	sess, ok := sessions.Get(sid)
	if ok {
		if sess.VNC != "" {
			log.Printf("[VNC_ENABLED] [%s]\n", sid)
			conn, err := net.Dial("tcp", sess.VNC)
			if err != nil {
				log.Printf("[VNC_ERROR] [%v]\n", err)
				return
			}
			defer conn.Close()
			wsconn.PayloadType = websocket.BinaryFrame
			go io.Copy(wsconn, conn)
			io.Copy(conn, wsconn)
			log.Printf("[VNC_CLIENT_DISCONNECTED] [%s]\n", sid)
		} else {
			log.Printf("[VNC_NOT_ENABLED] [%s]\n", sid)
		}
	} else {
		log.Printf("[SESSION_NOT_FOUND] [%s]\n", sid)
	}
}

func logs(wsconn *websocket.Conn) {
	defer wsconn.Close()
	sid := strings.Split(wsconn.Request().URL.Path, "/")[2]
	sess, ok := sessions.Get(sid)
	if ok && sess.Container != "" {
		log.Printf("[CONTAINER_LOGS] [%s]\n", sess.Container)
		r, err := cli.ContainerLogs(context.Background(), sess.Container, types.ContainerLogsOptions{
			ShowStdout: true,
			ShowStderr: true,
			Follow:     true,
		})
		if err != nil {
			log.Printf("[CONTAINER_LOGS_ERROR] [%v]\n", err)
			return
		}
		defer r.Close()
		wsconn.PayloadType = websocket.BinaryFrame
		io.Copy(wsconn, r)
		log.Printf("[WEBSOCCKET_CLIENT_DISCONNECTED] [%s]\n", sid)
	} else {
		log.Printf("[SESSION_NOT_FOUND] [%s]\n", sid)
	}
}

func onTimeout(t time.Duration, f func()) chan struct{} {
	cancel := make(chan struct{})
	go func(cancel chan struct{}) {
		select {
		case <-time.After(t):
			f()
		case <-cancel:
		}
	}(cancel)
	return cancel
}
