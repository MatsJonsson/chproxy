package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Vertamedia/chproxy/config"
	"github.com/Vertamedia/chproxy/log"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/crypto/acme/autocert"

	//client-go dependencies
	//"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
)

var (
	configFile = flag.String("config", "", "Proxy configuration filename")
	version    = flag.Bool("version", false, "Prints current version and exits")
)

var (
	proxy = newReverseProxy()

	// networks allow lists
	allowedNetworksHTTP    atomic.Value
	allowedNetworksHTTPS   atomic.Value
	allowedNetworksMetrics atomic.Value
)

// Clickhouse on Kubernetes means pods can come and go, i.e replicas and shards
// For this reason, CHproxy has been expanded to use the Client-go library to dynamically discover Clickhouse pods.
// Discovery is perfomed using the kubernetes APIs to find all pods in a cluster.
// Clickhouse pods are filtered out by means of comparing the pod name to strings included or excluded.

var clientset *kubernetes.Clientset // Kubernetes client API access

func main() {
	flag.Parse()
	if *version {
		fmt.Printf("%s\n", versionString())
		os.Exit(0)
	}

	log.Infof("%s", versionString())
	log.Infof("Loading config: %s", *configFile)
	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("error while loading config: %s", err)
	}
	if err = applyConfig(cfg); err != nil {
		log.Fatalf("error while applying config: %s", err)
	}
	log.Infof("Loading config %q: successful", *configFile)

	// Are we configured for dynamic kubernetes pod discovery?
	if cfg.Clusters[0].KubernetesPodDiscovery {

		// Are we inside or outside the Kubernetes cluster?
		var kubeaccessok bool
		kubeaccessok = true

		// Attempt to create an in-cluster config
		kconfig, err := rest.InClusterConfig()
		if err == nil {
			log.Infof("Failed to build cluster internal kubeconfig. No further kubernetes API access attempts will be made")
			clientset, err = kubernetes.NewForConfig(kconfig)
			if err != nil {
				log.Infof("Failed to create Kubernetes clientset for cluster internal accesss.")
				kubeaccessok = false

			}
		} else { // in-cluster config failed, attempt out of cluster config
			var kubeconfig *string
			if home := homedir.HomeDir(); home != "" {
				kubeconfig = flag.String("kubeconfig", filepath.Join(home, ".kube", "config"), "(optional) absolute path to the kubeconfig file")
			} else {
				kubeconfig = flag.String("kubeconfig", "", "absolute path to the kubeconfig file")
			}
			flag.Parse()

			// use the current context in kubeconfig
			kconfig, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
			if err != nil {
				log.Infof("Failed to build cluster external kubeconfig. Attempting to build cluster internal kubeconfig")
				kubeaccessok = false
			}
			clientset, err = kubernetes.NewForConfig(kconfig)
			if err != nil {
				log.Infof("Failed to create Kubernetes clientset for cluster internal accesss.")
				kubeaccessok = false
			}

		}

		// if we have Clickhouse deployed on kubernetes, and CHproxy has internal or external access to the Kubernetes APIs, we can automatically manage pods coming and going
		if kubeaccessok != false {
			kubepodscanticker := time.NewTicker(500 * time.Millisecond)

			go func() {

				for {

					select {
					case <-kubepodscanticker.C:
						{
							// fetch the configuration for kubenretes pod discovery
							chCurrentNodes := cfg.Clusters[0].Nodes
							sort.Strings(chCurrentNodes)
							skubeInclude := cfg.Clusters[0].KubernetesPodNameInclude
							skubeExclude := cfg.Clusters[0].KubernetesPodNameExclude

							// get pods in all the namespaces by omitting namespace
							pods, err := clientset.CoreV1().Pods("").List(context.TODO(), metav1.ListOptions{})
							if err != nil {
								log.Infof("Kubernetes API error: \n", err.Error())
							}

							//log.Infof("There are %d pods in the cluster\n", len(pods.Items))
							var chDiscoveredNodes []string

							// Filter out the relevant pods
							for i, s := range pods.Items {
								sname := s.GetName()
								_ = i //workaround for unused variable...

								if strings.Contains(sname, skubeInclude) && !strings.Contains(sname, skubeExclude) {
									//log.Infof("Clickhouse Pod %s   %s\n", i, s.GetName(), s.Status.PodIP)
									if len(s.Status.PodIP) > 6 {
										chDiscoveredNodes = append(chDiscoveredNodes, s.Status.PodIP+":8123")
									}
								}
							}
							sort.Strings(chDiscoveredNodes)

							// if pods appear of dissapear, there will be a difference between chCurrentNodes and chDiscoveredNodes
							if reflect.DeepEqual(chCurrentNodes, chDiscoveredNodes) == false {
								// Assign the new node configuration
								cfg.Clusters[0].Nodes = chDiscoveredNodes

								// Apply the updated configuration
								applyConfig(cfg)
								chCurrentNodes = chDiscoveredNodes

								log.Infof("Clickhouse Pod configuration changed, node list updated\n")
							}
						}
					}
				}
			}() //end gofunc
		}
	} // end of kubernetes pod discovery
	//////////////

	c := make(chan os.Signal)
	signal.Notify(c, syscall.SIGHUP)
	go func() {
		for {
			switch <-c {
			case syscall.SIGHUP:
				log.Infof("SIGHUP received. Going to reload config %s ...", *configFile)
				if err := reloadConfig(); err != nil {
					log.Errorf("error while reloading config: %s", err)
					continue
				}
				log.Infof("Reloading config %s: successful", *configFile)
			}
		}
	}()

	server := cfg.Server
	if len(server.HTTP.ListenAddr) == 0 && len(server.HTTPS.ListenAddr) == 0 {
		panic("BUG: broken config validation - `listen_addr` is not configured")
	}

	if server.HTTP.ForceAutocertHandler {
		autocertManager = newAutocertManager(server.HTTPS.Autocert)
	}
	if len(server.HTTPS.ListenAddr) != 0 {
		go serveTLS(server.HTTPS)
	}
	if len(server.HTTP.ListenAddr) != 0 {
		go serve(server.HTTP)
	}

	select {}
}

var autocertManager *autocert.Manager

func newAutocertManager(cfg config.Autocert) *autocert.Manager {
	if len(cfg.CacheDir) > 0 {
		if err := os.MkdirAll(cfg.CacheDir, 0700); err != nil {
			log.Fatalf("error while creating folder %q: %s", cfg.CacheDir, err)
		}
	}
	var hp autocert.HostPolicy
	if len(cfg.AllowedHosts) != 0 {
		allowedHosts := make(map[string]struct{}, len(cfg.AllowedHosts))
		for _, v := range cfg.AllowedHosts {
			allowedHosts[v] = struct{}{}
		}
		hp = func(_ context.Context, host string) error {
			if _, ok := allowedHosts[host]; ok {
				return nil
			}
			return fmt.Errorf("host %q doesn't match `host_policy` configuration", host)
		}
	}
	return &autocert.Manager{
		Prompt:     autocert.AcceptTOS,
		Cache:      autocert.DirCache(cfg.CacheDir),
		HostPolicy: hp,
	}
}

func newListener(listenAddr string) net.Listener {
	ln, err := net.Listen("tcp4", listenAddr)
	if err != nil {
		log.Fatalf("cannot listen for %q: %s", listenAddr, err)
	}
	return ln
}

func serveTLS(cfg config.HTTPS) {
	ln := newListener(cfg.ListenAddr)
	h := http.HandlerFunc(serveHTTP)
	tlsCfg := newTLSConfig(cfg)
	tln := tls.NewListener(ln, tlsCfg)
	log.Infof("Serving https on %q", cfg.ListenAddr)
	if err := listenAndServe(tln, h, cfg.TimeoutCfg); err != nil {
		log.Fatalf("TLS server error on %q: %s", cfg.ListenAddr, err)
	}
}

func serve(cfg config.HTTP) {
	var h http.Handler
	ln := newListener(cfg.ListenAddr)
	h = http.HandlerFunc(serveHTTP)
	if cfg.ForceAutocertHandler {
		if autocertManager == nil {
			panic("BUG: autocertManager is not inited")
		}
		addr := ln.Addr().String()
		parts := strings.Split(addr, ":")
		if parts[len(parts)-1] != "80" {
			log.Fatalf("`letsencrypt` specification requires http server to listen on :80 port to satisfy http-01 challenge. " +
				"Otherwise, certificates will be impossible to generate")
		}
		h = autocertManager.HTTPHandler(h)
	}
	log.Infof("Serving http on %q", cfg.ListenAddr)
	if err := listenAndServe(ln, h, cfg.TimeoutCfg); err != nil {
		log.Fatalf("HTTP server error on %q: %s", cfg.ListenAddr, err)
	}
}

func newTLSConfig(cfg config.HTTPS) *tls.Config {
	tlsCfg := tls.Config{
		PreferServerCipherSuites: true,
		CurvePreferences: []tls.CurveID{
			tls.CurveP256,
			tls.X25519,
		},
	}
	if len(cfg.KeyFile) > 0 && len(cfg.CertFile) > 0 {
		cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
		if err != nil {
			log.Fatalf("cannot load cert for `https.cert_file`=%q, `https.key_file`=%q: %s",
				cfg.CertFile, cfg.KeyFile, err)
		}
		tlsCfg.Certificates = []tls.Certificate{cert}
	} else {
		if autocertManager == nil {
			panic("BUG: autocertManager is not inited")
		}
		tlsCfg.GetCertificate = autocertManager.GetCertificate
	}
	return &tlsCfg
}

func listenAndServe(ln net.Listener, h http.Handler, cfg config.TimeoutCfg) error {
	s := &http.Server{
		TLSNextProto: make(map[string]func(*http.Server, *tls.Conn, http.Handler)),
		Handler:      h,
		ReadTimeout:  time.Duration(cfg.ReadTimeout),
		WriteTimeout: time.Duration(cfg.WriteTimeout),
		IdleTimeout:  time.Duration(cfg.IdleTimeout),

		// Suppress error logging from the server, since chproxy
		// must handle all these errors in the code.
		ErrorLog: log.NilLogger,
	}
	return s.Serve(ln)
}

var promHandler = promhttp.Handler()

func serveHTTP(rw http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodPost:
		// Only GET and POST methods are supported.
	case http.MethodOptions:
		// This is required for CORS shit :)
		rw.Header().Set("Allow", "GET,POST")
		return
	default:
		err := fmt.Errorf("%q: unsupported method %q", r.RemoteAddr, r.Method)
		rw.Header().Set("Connection", "close")
		respondWith(rw, err, http.StatusMethodNotAllowed)
		return
	}

	switch r.URL.Path {
	case "/favicon.ico":
	case "/metrics":
		an := allowedNetworksMetrics.Load().(*config.Networks)
		if !an.Contains(r.RemoteAddr) {
			err := fmt.Errorf("connections to /metrics are not allowed from %s", r.RemoteAddr)
			rw.Header().Set("Connection", "close")
			respondWith(rw, err, http.StatusForbidden)
			return
		}
		proxy.refreshCacheMetrics()
		promHandler.ServeHTTP(rw, r)
	case "/", "/query":
		var err error
		var an *config.Networks
		if r.TLS != nil {
			an = allowedNetworksHTTPS.Load().(*config.Networks)
			err = fmt.Errorf("https connections are not allowed from %s", r.RemoteAddr)
		} else {
			an = allowedNetworksHTTP.Load().(*config.Networks)
			err = fmt.Errorf("http connections are not allowed from %s", r.RemoteAddr)
		}
		if !an.Contains(r.RemoteAddr) {
			rw.Header().Set("Connection", "close")
			respondWith(rw, err, http.StatusForbidden)
			return
		}
		proxy.ServeHTTP(rw, r)
	default:
		badRequest.Inc()
		err := fmt.Errorf("%q: unsupported path: %q", r.RemoteAddr, r.URL.Path)
		rw.Header().Set("Connection", "close")
		respondWith(rw, err, http.StatusBadRequest)
	}
}

func loadConfig() (*config.Config, error) {
	if *configFile == "" {
		log.Fatalf("Missing -config flag")
	}
	cfg, err := config.LoadFile(*configFile)
	if err != nil {
		configSuccess.Set(0)
		return nil, fmt.Errorf("can't load config %q: %s", *configFile, err)
	}
	configSuccess.Set(1)
	configSuccessTime.Set(float64(time.Now().Unix()))
	return cfg, nil
}

func applyConfig(cfg *config.Config) error {
	if err := proxy.applyConfig(cfg); err != nil {
		return err
	}
	allowedNetworksHTTP.Store(&cfg.Server.HTTP.AllowedNetworks)
	allowedNetworksHTTPS.Store(&cfg.Server.HTTPS.AllowedNetworks)
	allowedNetworksMetrics.Store(&cfg.Server.Metrics.AllowedNetworks)
	log.SetDebug(cfg.LogDebug)
	log.Infof("Loaded config:\n%s", cfg)

	return nil
}

func reloadConfig() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	return applyConfig(cfg)
}

var (
	buildTag      = "unknown"
	buildRevision = "unknown"
	buildTime     = "unknown"
)

func versionString() string {
	ver := buildTag
	if len(ver) == 0 {
		ver = "unknown"
	}
	return fmt.Sprintf("chproxy ver. %s, rev. %s, built at %s", ver, buildRevision, buildTime)
}
