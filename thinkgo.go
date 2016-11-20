// Copyright 2016 HenryLee. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package thinkgo

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/henrylee2cn/thinkgo/logging"
	"github.com/henrylee2cn/thinkgo/session"
	"github.com/henrylee2cn/thinkgo/swagger"
	"github.com/henrylee2cn/thinkgo/utils/errors"
	"github.com/rsc/letsencrypt"
	// "github.com/facebookgo/grace/gracehttp"
)

const (
	VERSION = "0.1"
	banner  = `
   _______  _                _                    
  |__   __|| |    [ ]       | |                   
     | |   | |_    _    _   | |  _   ___    ___   
     | |   |  _ \ | | / _ \ | |/ /  / _ \  / _ \  
     | |   | | | || || | | ||   -  | |_| || |_| | 
     |_|   |_| |_||_||_| |_||_| \_\ \_  /  \___/  
                                    _ \ \         
                                    \_\_/         ` + VERSION + "\n"
)

// Thinkgo web framework.
type Framework struct {
	name           string // name of the application
	version        string // version of the application
	config         Config
	*MuxAPI        // root muxAPI node
	muxesForRouter MuxAPIs
	server         *http.Server
	once           sync.Once
	sessionManager *session.Manager
	syslog         *logging.Logger // for framework
	bizlog         *logging.Logger // for user bissness
	apidoc         *swagger.Swagger
}

// Use the thinkgo web framework to create a new application.
func New(name string, version ...string) *Framework {
	mutexNewApp.Lock()
	defer mutexNewApp.Unlock()
	configFileName, ver := createConfigFilenameAndVersion(name, version...)
	frame := &Framework{
		name:           name,
		version:        ver,
		muxesForRouter: nil,
		config:         newConfig(configFileName),
	}
	frame.initSysLogger()
	frame.initBizLogger()
	frame.MuxAPI = newMuxAPI(frame, "root", "", "/")

	id := frame.NameWithVersion()
	if _, ok := Apps[id]; ok {
		Fatalf("There are two applications with exactly the same name and version: %s", id)
	}

	Apps[frame.NameWithVersion()] = frame

	return frame
}

var (
	// The list of applications that have been created.
	Apps        = make(map[string]*Framework)
	mutexNewApp sync.Mutex

	// Make sure that the initialization logs for multiple applications are printed in sequence
	mutexForRun sync.Mutex
)

// name of the application
func (frame *Framework) Name() string {
	return frame.name
}

// version of the application
func (frame *Framework) Version() string {
	return frame.version
}

func (frame *Framework) NameWithVersion() string {
	if len(frame.version) == 0 {
		return frame.name
	}
	return frame.name + "_" + frame.version
}

// Start web service.
func (frame *Framework) Run() {
	// Make sure that the initialization logs for multiple applications are printed in sequence
	mutexForRun.Lock()

	frame.build()
	var err error
	var protocol = "HTTP"
	switch frame.config.NetType {
	case NETTYPE_NORMAL:
		frame.syslog.Criticalf("[%s] listen and serve %s/HTTP2 on %v", frame.NameWithVersion(), protocol, frame.config.Addr)
		mutexForRun.Unlock()
		err = frame.listenAndServe()
	case NETTYPE_TLS:
		protocol = "HTTPS"
		frame.syslog.Criticalf("[%s] listen and serve %s/HTTP2 on %v", frame.NameWithVersion(), protocol, frame.config.Addr)
		mutexForRun.Unlock()
		err = frame.listenAndServeTLS(frame.config.TLSCertFile, frame.config.TLSKeyFile)
	case NETTYPE_LETSENCRYPT:
		protocol = "HTTPS"
		frame.syslog.Criticalf("[%s] listen and serve %s/HTTP2 on %v (pid:%d)", frame.NameWithVersion(), protocol, frame.config.Addr, os.Getpid())
		mutexForRun.Unlock()
		err = frame.listenAndServeLETSENCRYPT(frame.config.LetsencryptFile)
	case NETTYPE_UNIX:
		frame.syslog.Criticalf("[%s] listen and serve %s/HTTP2 on %v (pid:%d)", frame.NameWithVersion(), protocol, frame.config.Addr, os.Getpid())
		mutexForRun.Unlock()
		err = frame.listenAndServeUNIX(frame.config.UNIXFileMode)
	default:
		mutexForRun.Unlock()
		frame.syslog.Fatal("Please set a valid config item net_type, refer to the following:\nnormal | tls | letsencrypt | unix")
	}
	if err != nil {
		frame.syslog.Fatal(err)
	}
}

// listenAndServe listens on the TCP network address and then
// calls Serve to handle requests on incoming connections.
// Accepted connections are configured to enable TCP keep-alives.
// If srv.Addr is blank, ":http" is used, listenAndServe always returns a non-nil error.
func (frame *Framework) listenAndServe() error {
	return frame.server.ListenAndServe()
}

// listenAndServeTLS listens on the TCP network address and
// then calls Serve to handle requests on incoming TLS connections.
// Accepted connections are configured to enable TCP keep-alives.
//
// Filenames containing a certificate and matching private key for the
// server must be provided if neither the Server's TLSConfig.Certificates
// nor TLSConfig.GetCertificate are populated. If the certificate is
// signed by a certificate authority, the certFile should be the
// concatenation of the server's certificate, any intermediates, and
// the CA's certificate.
//
// If frame.config.Addr is blank, ":https" is used, listenAndServeTLS always returns a non-nil error.
func (frame *Framework) listenAndServeTLS(certFile, keyFile string) error {
	return frame.server.ListenAndServeTLS(certFile, keyFile)
}

// listenAndServeLETSENCRYPT listens on a new Automatic TLS using letsencrypt.org service.
// if you want to disable cache file then simple give cacheFileOptional a value of empty string ""
func (frame *Framework) listenAndServeLETSENCRYPT(cacheFileOptional string) error {
	if frame.server.Addr == "" {
		frame.server.Addr = ":https"
	}

	ln, err := net.Listen("tcp4", frame.server.Addr)
	if err != nil {
		return err
	}

	var m letsencrypt.Manager
	if cacheFileOptional != "" {
		if err = m.CacheFile(cacheFileOptional); err != nil {
			return err
		}
	}

	tlsConfig := &tls.Config{GetCertificate: m.GetCertificate}
	tlsListener := tls.NewListener(tcpKeepAliveListener{ln.(*net.TCPListener)}, tlsConfig)

	return frame.server.Serve(tlsListener)
}

var (
	errPortAlreadyUsed = errors.New("Port is already used")
	errRemoveUnix      = errors.New("Unexpected error when trying to remove unix socket file. Addr: %s | Trace: %s")
	errChmod           = errors.New("Cannot chmod %#o for %q: %s")
	errCertKeyMissing  = errors.New("You should provide certFile and keyFile for TLS/SSL")
	errParseTLS        = errors.New("Couldn't load TLS, certFile=%q, keyFile=%q. Trace: %s")
)

// listenAndServeUNIX announces on the Unix domain socket laddr and listens a Unix service.
func (frame *Framework) listenAndServeUNIX(fileMode os.FileMode) error {
	if errOs := os.Remove(frame.server.Addr); errOs != nil && !os.IsNotExist(errOs) {
		return errRemoveUnix.Format(frame.server.Addr, errOs.Error())
	}

	ln, err := net.Listen("unix", frame.server.Addr)
	if err != nil {
		return errPortAlreadyUsed.AppendErr(err)
	}

	if err = os.Chmod(frame.server.Addr, fileMode); err != nil {
		return errChmod.Format(fileMode, frame.server.Addr, err.Error())
	}
	return frame.server.Serve(tcpKeepAliveListener{ln.(*net.TCPListener)})
}

// tcpKeepAliveListener sets TCP keep-alive timeouts on accepted
// connections. It's used by ListenAndServe and ListenAndServeTLS so
// dead TCP connections (e.g. closing laptop mid-download) eventually
// go away.
type tcpKeepAliveListener struct {
	*net.TCPListener
}

func (ln tcpKeepAliveListener) Accept() (c net.Conn, err error) {
	tc, err := ln.AcceptTCP()
	if err != nil {
		return
	}
	tc.SetKeepAlive(true)
	tc.SetKeepAlivePeriod(3 * time.Minute)
	return tc, nil
}

func (frame *Framework) build() {
	frame.once.Do(func() {
		// register the default MuxAPIs
		{
			// apidoc
			if frame.config.APIdoc.Enable {
				frame.regAPIdoc()
			}
			// static
			frame.presetSystemMuxes()
		}

		// build router
		var router = &Router{
			RedirectTrailingSlash:  frame.config.Router.RedirectTrailingSlash,
			RedirectFixedPath:      frame.config.Router.RedirectFixedPath,
			HandleMethodNotAllowed: frame.config.Router.HandleMethodNotAllowed,
			HandleOPTIONS:          frame.config.Router.HandleOPTIONS,
			NotFound:               frame.makeErrorHandler(http.StatusNotFound),
			MethodNotAllowed:       frame.makeErrorHandler(http.StatusMethodNotAllowed),
			PanicHandler:           frame.makePanicHandler(),
		}

		// register router
		for _, node := range frame.MuxAPIsForRouter() {
			handle := frame.makeHandle(node.handlers)
			for _, method := range node.methods {

				frame.syslog.Criticalf("%7s | %-30s", method, node.path)

				router.Handle(method, node.path, handle)
			}
		}

		// new server
		frame.server = &http.Server{
			Addr:         frame.config.Addr,
			Handler:      router,
			ReadTimeout:  frame.config.ReadTimeout,
			WriteTimeout: frame.config.WriteTimeout,
		}

		// register session
		frame.registerSession()
	})
}

// The log used by the user bissness
func (frame *Framework) Log() *logging.Logger {
	return frame.bizlog
}

// Get an ordered list of nodes used to register router.
func (frame *Framework) MuxAPIsForRouter() []*MuxAPI {
	if frame.muxesForRouter == nil {
		// comb mux.handlers, mux.paramInfos, mux.returns and mux.path,.
		frame.MuxAPI.comb()

		frame.muxesForRouter = frame.MuxAPI.HandlerProgeny()
	}
	return frame.muxesForRouter
}

// Append middlewares of function type to root muxAPI.
// Used to register router in tree style.
func (frame *Framework) Route(children ...*MuxAPI) *MuxAPI {
	frame.MuxAPI.children = append(frame.MuxAPI.children, children...)
	for _, child := range children {
		child.parent = frame.MuxAPI
	}
	return frame.MuxAPI
}

// NewGroup create an isolated grouping muxAPI node.
func (frame *Framework) NewGroup(pattern string, children ...*MuxAPI) *MuxAPI {
	return frame.NewNamedGroup("", pattern, children...)
}

// NewAPI creates an isolated muxAPI node.
func (frame *Framework) NewAPI(methodset Methodset, pattern string, handlers ...Handler) *MuxAPI {
	return frame.NewNamedAPI("", methodset, pattern, handlers...)
}

// NewNamedGroup creates an isolated grouping muxAPI node with the name.
func (frame *Framework) NewNamedGroup(name string, pattern string, children ...*MuxAPI) *MuxAPI {
	group := frame.NewNamedAPI(name, "", pattern)
	group.children = append(group.children, children...)
	for _, child := range children {
		child.parent = group
	}
	return group
}

// NewNamedAPI creates an isolated muxAPI node with the name.
func (frame *Framework) NewNamedAPI(name string, methodset Methodset, pattern string, handlers ...Handler) *MuxAPI {
	return newMuxAPI(frame, name, methodset, pattern, handlers...)
}

// NewGET is a shortcut for frame.NewAPI("GET", pattern, handlers...)
func (frame *Framework) NewGET(pattern string, handlers ...Handler) *MuxAPI {
	return frame.NewAPI("GET", pattern, handlers...)
}

// NewHEAD is a shortcut for frame.NewAPI("HEAD", pattern, handlers...)
func (frame *Framework) NewHEAD(pattern string, handlers ...Handler) *MuxAPI {
	return frame.NewAPI("HEAD", pattern, handlers...)
}

// NewOPTIONS is a shortcut for frame.NewAPI("OPTIONS", pattern, handlers...)
func (frame *Framework) NewOPTIONS(pattern string, handlers ...Handler) *MuxAPI {
	return frame.NewAPI("OPTIONS", pattern, handlers...)
}

// NewPOST is a shortcut for frame.NewAPI("POST", pattern, handlers...)
func (frame *Framework) NewPOST(pattern string, handlers ...Handler) *MuxAPI {
	return frame.NewAPI("POST", pattern, handlers...)
}

// NewPUT is a shortcut for frame.NewAPI("PUT", pattern, handlers...)
func (frame *Framework) NewPUT(pattern string, handlers ...Handler) *MuxAPI {
	return frame.NewAPI("PUT", pattern, handlers...)
}

// NewPATCH is a shortcut for frame.NewAPI("PATCH", pattern, handlers...)
func (frame *Framework) NewPATCH(pattern string, handlers ...Handler) *MuxAPI {
	return frame.NewAPI("PATCH", pattern, handlers...)
}

// NewDELETE is a shortcut for frame.NewAPI("DELETE", pattern, handlers...)
func (frame *Framework) NewDELETE(pattern string, handlers ...Handler) *MuxAPI {
	return frame.NewAPI("DELETE", pattern, handlers...)
}

// NewNamedGET is a shortcut for frame.NewNamedAPI(name, "GET", pattern, handlers...)
func (frame *Framework) NewNamedGET(name string, pattern string, handlers ...Handler) *MuxAPI {
	return frame.NewNamedAPI(name, "GET", pattern, handlers...)
}

// NewNamedHEAD is a shortcut for frame.NewNamedAPI(name, "HEAD", pattern, handlers...)
func (frame *Framework) NewNamedHEAD(name string, pattern string, handlers ...Handler) *MuxAPI {
	return frame.NewNamedAPI(name, "HEAD", pattern, handlers...)
}

// NewNamedOPTIONS is a shortcut for frame.NewNamedAPI(name, "OPTIONS", pattern, handlers...)
func (frame *Framework) NewNamedOPTIONS(name string, pattern string, handlers ...Handler) *MuxAPI {
	return frame.NewNamedAPI(name, "OPTIONS", pattern, handlers...)
}

// NewNamedPOST is a shortcut for frame.NewNamedAPI(name, "POST", pattern, handlers...)
func (frame *Framework) NewNamedPOST(name string, pattern string, handlers ...Handler) *MuxAPI {
	return frame.NewNamedAPI(name, "POST", pattern, handlers...)
}

// NewNamedPUT is a shortcut for frame.NewNamedAPI(name, "PUT", pattern, handlers...)
func (frame *Framework) NewNamedPUT(name string, pattern string, handlers ...Handler) *MuxAPI {
	return frame.NewNamedAPI(name, "PUT", pattern, handlers...)
}

// NewNamedPATCH is a shortcut for frame.NewNamedAPI(name, "PATCH", pattern, handlers...)
func (frame *Framework) NewNamedPATCH(name string, pattern string, handlers ...Handler) *MuxAPI {
	return frame.NewNamedAPI(name, "PATCH", pattern, handlers...)
}

// NewNamedDELETE is a shortcut for frame.NewNamedAPI(name, "DELETE", pattern, handlers...)
func (frame *Framework) NewNamedDELETE(name string, pattern string, handlers ...Handler) *MuxAPI {
	return frame.NewNamedAPI(name, "DELETE", pattern, handlers...)
}

// NewNamedStatic creates an isolated static muxAPI node.
func (frame *Framework) NewStatic(pattern string, root string) *MuxAPI {
	return frame.NewNamedStatic("", pattern, root)
}

// NewNamedStatic creates an isolated static muxAPI node with the name.
func (frame *Framework) NewNamedStatic(name, pattern string, root string) *MuxAPI {
	return (&MuxAPI{frame: frame}).NamedStatic(name, pattern, root)
}

// NewNamedStatic creates an isolated static muxAPI node.
func (frame *Framework) NewStaticFS(pattern string, fs http.FileSystem) *MuxAPI {
	return frame.NewNamedStaticFS("", pattern, fs)
}

// NewNamedStatic creates an isolated static muxAPI node with the name.
func (frame *Framework) NewNamedStaticFS(name, pattern string, fs http.FileSystem) *MuxAPI {
	return (&MuxAPI{frame: frame}).NamedStaticFS(name, pattern, fs)
}

// makeHandle makes an *apiware.ParamsAPI implements the Handle interface.
func (frame *Framework) makeHandle(handlerChain HandlerChain) Handle {
	ctxPool := sync.Pool{
		New: func() interface{} {
			return newContext(frame, handlerChain)
		},
	}
	return func(w http.ResponseWriter, r *http.Request, pathParams Params) {
		ctx := ctxPool.Get().(*Context)
		ctx.reset(w, r, pathParams)
		defer func() {
			ctxPool.Put(ctx)
		}()
		ctx.start()
	}
}

// Create the handle to be called by the router
func (frame *Framework) makeErrorHandler(status int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		Global.errorFunc(newEmptyContext(frame, w, r), http.StatusText(status), status)
	})
}

// Create the handle to be called by the router
func (frame *Framework) makePanicHandler() func(http.ResponseWriter, *http.Request, interface{}) {
	s := []byte("/src/runtime/panic.go")
	e := []byte("\ngoroutine ")
	line := []byte("\n")
	return func(w http.ResponseWriter, r *http.Request, rcv interface{}) {
		stack := make([]byte, 4<<10) //4KB
		length := runtime.Stack(stack, true)
		start := bytes.Index(stack, s)
		stack = stack[start:length]
		start = bytes.Index(stack, line) + 1
		stack = stack[start:]
		if end := bytes.Index(stack, e); end != -1 {
			stack = stack[:end]
		}
		errStr := fmt.Sprintf("%v\n\n[TRACE]\n%s", rcv, stack)
		Global.errorFunc(newEmptyContext(frame, w, r), errStr, http.StatusInternalServerError)
	}
}

func (frame *Framework) presetSystemMuxes() {
	frame.Use(AccessLogWare())
	frame.MuxAPI.NamedStatic("Directory for uploading files", "/upload/", Global.uploadDir)
	frame.MuxAPI.NamedStatic("Directory for public static files", "/static/", Global.staticDir)
}

func (frame *Framework) registerSession() {
	if !frame.config.Session.Enable {
		return
	}
	conf := &session.ManagerConfig{
		CookieName:              frame.config.Session.Name,
		EnableSetCookie:         frame.config.Session.AutoSetCookie,
		Gclifetime:              frame.config.Session.GCMaxLifetime,
		Secure:                  frame.config.NetType == "tls" || frame.config.NetType == "letsencrypt",
		CookieLifeTime:          frame.config.Session.CookieLifetime,
		ProviderConfig:          frame.config.Session.ProviderConfig,
		Domain:                  frame.config.Session.Domain,
		EnableSidInHttpHeader:   frame.config.Session.EnableSidInHttpHeader,
		SessionNameInHttpHeader: frame.config.Session.NameInHttpHeader,
		EnableSidInUrlQuery:     frame.config.Session.EnableSidInUrlQuery,
	}
	var err error
	frame.sessionManager, err = session.NewManager(frame.config.Session.Provider, conf)
	if err != nil {
		panic(err)
	}
	go frame.sessionManager.GC()
}

func createConfigFilenameAndVersion(name string, version ...string) (fileName string, ver string) {
	if len(version) > 0 && len(version[0]) > 0 {
		ver = version[0]
		fileName = CONFIG_DIR + "/" + name + "_" + ver + ".ini"
	} else {
		fileName = CONFIG_DIR + "/" + name + ".ini"
	}
	return
}
