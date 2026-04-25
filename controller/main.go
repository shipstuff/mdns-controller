package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	defaultSuffix            = ".local"
	defaultExcludeAnnotation = "mdns.shipstuff.io/enabled"
	defaultHTTPAddr          = ":8080"
	defaultReconcileInterval = 15 * time.Second
	defaultStaleAfter        = 60 * time.Second
	publishStartTimeout      = 1500 * time.Millisecond
	serviceAccountRoot       = "/var/run/secrets/kubernetes.io/serviceaccount"
)

type config struct {
	Address           string
	AnnotationKey     string
	DefaultEnabled    bool
	HTTPSAddr         string
	HostSuffix        string
	InterfaceName     string
	ReconcileInterval time.Duration
	StaleAfter        time.Duration
}

type controllerState struct {
	mu                  sync.RWMutex
	accountedHash       string
	collidedHosts       []string
	dbusConnected       bool
	desiredHash         string
	desiredHosts        []string
	initialSyncDone     bool
	lastError           string
	lastReconcile       time.Time
	lastSuccess         time.Time
	publishedHash       string
	publishedHosts      []string
	reconcileErrorTotal uint64
	reconcileOKTotal    uint64
	staleAfter          time.Duration
}

type ingressList struct {
	Metadata listMetadata `json:"metadata"`
	Items    []ingress    `json:"items"`
}

type listMetadata struct {
	Continue        string `json:"continue"`
	ResourceVersion string `json:"resourceVersion"`
}

type ingress struct {
	Metadata objectMeta  `json:"metadata"`
	Spec     ingressSpec `json:"spec"`
}

type objectMeta struct {
	Name        string            `json:"name"`
	Namespace   string            `json:"namespace"`
	Annotations map[string]string `json:"annotations"`
}

type ingressSpec struct {
	Rules []ingressRule `json:"rules"`
}

type ingressRule struct {
	Host string `json:"host"`
}

type publishedProcess struct {
	cmd    *exec.Cmd
	host   string
	output *bytes.Buffer
	waitCh chan error
}

type avahiPublisher struct {
	address string
	procs   map[string]*publishedProcess
}

type kubeClient struct {
	baseURL    string
	bearer     string
	httpClient *http.Client
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	state := &controllerState{staleAfter: cfg.StaleAfter}
	go serveHTTP(cfg.HTTPSAddr, state)

	client, err := newKubeClient()
	if err != nil {
		log.Fatalf("kube client: %v", err)
	}

	ifaceName := cfg.InterfaceName
	if ifaceName == "" {
		ifaceName, err = detectInterface()
		if err != nil {
			log.Fatalf("detect interface: %v", err)
		}
	}

	address := cfg.Address
	if address == "" {
		address, err = detectAddress(ifaceName)
		if err != nil {
			log.Fatalf("detect address: %v", err)
		}
	}

	log.Printf("starting mdns controller on interface=%s address=%s suffix=%s annotation_key=%s default_enabled=%t interval=%s stale_after=%s",
		ifaceName, address, cfg.HostSuffix, cfg.AnnotationKey, cfg.DefaultEnabled, cfg.ReconcileInterval, cfg.StaleAfter)

	for {
		publisher, err := newAvahiPublisher(address)
		if err != nil {
			state.markDBusError(err)
			log.Printf("publisher init failed: %v", err)
			time.Sleep(5 * time.Second)
			continue
		}

		state.setDBusConnected(true)

		runErr := runLoop(client, publisher, state, cfg)
		_ = publisher.Close()
		state.setDBusConnected(false)
		if runErr != nil {
			log.Printf("controller loop restarting after fatal error: %v", runErr)
			time.Sleep(5 * time.Second)
		}
	}
}

func runLoop(client *kubeClient, publisher *avahiPublisher, state *controllerState, cfg config) error {
	ticker := time.NewTicker(cfg.ReconcileInterval)
	defer ticker.Stop()

	for {
		if err := reconcileOnce(client, publisher, state, cfg); err != nil {
			state.markReconcileError(err)
			log.Printf("reconcile failed, keeping last published state: %v", err)
		}
		<-ticker.C
	}
}

func reconcileOnce(client *kubeClient, publisher *avahiPublisher, state *controllerState, cfg config) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	hosts, err := client.ListIngressHosts(ctx, cfg.HostSuffix, cfg.AnnotationKey, cfg.DefaultEnabled)
	if err != nil {
		return fmt.Errorf("list ingress hosts: %w", err)
	}

	hash := hashHosts(hosts)
	state.setDesired(hosts, hash)

	prevPublished := state.getPublishedHosts()
	prevCollided := state.getCollidedHosts()

	published, collided, err := publisher.Apply(hosts)
	if err != nil {
		return fmt.Errorf("apply avahi state: %w", err)
	}

	if !sameStringSlices(prevPublished, published) || !sameStringSlices(prevCollided, collided) {
		logPublishedState(published, collided)
		if len(collided) > 0 {
			log.Printf("skipping %d colliding mDNS aliases: %s", len(collided), strings.Join(collided, ", "))
		}
	}

	state.markReconcileSuccess(hosts, published, collided, hash)
	return nil
}

func loadConfig() (config, error) {
	cfg := config{
		Address:       strings.TrimSpace(os.Getenv("MDNS_ADDRESS")),
		AnnotationKey: getenvDefault("MDNS_ENABLED_ANNOTATION", getenvDefault("MDNS_EXCLUDE_ANNOTATION", defaultExcludeAnnotation)),
		HTTPSAddr:     getenvDefault("MDNS_HTTP_ADDR", defaultHTTPAddr),
		HostSuffix:    strings.ToLower(getenvDefault("MDNS_HOST_SUFFIX", defaultSuffix)),
		InterfaceName: strings.TrimSpace(os.Getenv("MDNS_INTERFACE")),
	}

	var err error
	cfg.ReconcileInterval, err = parseDurationEnv("MDNS_RECONCILE_INTERVAL", defaultReconcileInterval)
	if err != nil {
		return config{}, err
	}
	cfg.StaleAfter, err = parseDurationEnv("MDNS_STALE_AFTER", defaultStaleAfter)
	if err != nil {
		return config{}, err
	}
	cfg.DefaultEnabled, err = parseBoolEnv("MDNS_DEFAULT_ENABLED", true)
	if err != nil {
		return config{}, err
	}

	if !strings.HasPrefix(cfg.HostSuffix, ".") {
		cfg.HostSuffix = "." + cfg.HostSuffix
	}

	return cfg, nil
}

func newKubeClient() (*kubeClient, error) {
	host := strings.TrimSpace(os.Getenv("KUBERNETES_SERVICE_HOST"))
	port := strings.TrimSpace(os.Getenv("KUBERNETES_SERVICE_PORT_HTTPS"))
	if host == "" || port == "" {
		return nil, errors.New("missing kubernetes service host/port env")
	}

	token, err := os.ReadFile(filepath.Join(serviceAccountRoot, "token"))
	if err != nil {
		return nil, fmt.Errorf("read service account token: %w", err)
	}

	caPEM, err := os.ReadFile(filepath.Join(serviceAccountRoot, "ca.crt"))
	if err != nil {
		return nil, fmt.Errorf("read service account ca: %w", err)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("append kubernetes ca cert")
	}

	return &kubeClient{
		baseURL: fmt.Sprintf("https://%s:%s", host, port),
		bearer:  strings.TrimSpace(string(token)),
		httpClient: &http.Client{
			Timeout: 20 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{RootCAs: pool},
			},
		},
	}, nil
}

func (c *kubeClient) ListIngressHosts(ctx context.Context, suffix, annotationKey string, defaultEnabled bool) ([]string, error) {
	hosts := make(map[string]struct{})
	continueToken := ""

	for {
		apiURL, err := url.Parse(c.baseURL + "/apis/networking.k8s.io/v1/ingresses")
		if err != nil {
			return nil, err
		}
		query := apiURL.Query()
		query.Set("limit", "500")
		if continueToken != "" {
			query.Set("continue", continueToken)
		}
		apiURL.RawQuery = query.Encode()

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL.String(), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+c.bearer)
		req.Header.Set("Accept", "application/json")

		resp, err := c.httpClient.Do(req)
		if err != nil {
			return nil, err
		}

		body, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil {
			return nil, readErr
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("unexpected status %s: %s", resp.Status, strings.TrimSpace(string(body)))
		}

		var list ingressList
		if err := json.Unmarshal(body, &list); err != nil {
			return nil, fmt.Errorf("decode ingress list: %w", err)
		}

		for _, item := range list.Items {
			if !annotationEnabled(item.Metadata.Annotations, annotationKey, defaultEnabled) {
				continue
			}
			for _, rule := range item.Spec.Rules {
				host := strings.ToLower(strings.TrimSpace(rule.Host))
				if strings.HasSuffix(host, suffix) {
					hosts[host] = struct{}{}
				}
			}
		}

		if list.Metadata.Continue == "" {
			break
		}
		continueToken = list.Metadata.Continue
	}

	out := make([]string, 0, len(hosts))
	for host := range hosts {
		out = append(out, host)
	}
	sort.Strings(out)
	return out, nil
}

func logPublishedState(published []string, collided []string) {
	if len(published) == 0 {
		log.Printf("published 0 mDNS aliases")
	} else {
		log.Printf("published %d mDNS aliases: %s", len(published), strings.Join(published, ", "))
	}
	if len(collided) > 0 {
		log.Printf("collided %d mDNS aliases: %s", len(collided), strings.Join(collided, ", "))
	}
}

func annotationEnabled(annotations map[string]string, key string, defaultEnabled bool) bool {
	value, ok := annotations[key]
	if !ok {
		return defaultEnabled
	}
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "true", "1", "yes", "on", "enable", "enabled":
		return true
	case "false", "0", "no", "off", "disable", "disabled":
		return false
	default:
		return defaultEnabled
	}
}

func newAvahiPublisher(address string) (*avahiPublisher, error) {
	if _, err := exec.LookPath("avahi-publish-address"); err != nil {
		return nil, err
	}
	return &avahiPublisher{
		address: address,
		procs:   make(map[string]*publishedProcess),
	}, nil
}

func (p *avahiPublisher) Apply(hosts []string) ([]string, []string, error) {
	p.reapExited()

	desired := make(map[string]struct{}, len(hosts))
	for _, host := range hosts {
		if host == "" {
			continue
		}
		desired[host] = struct{}{}
	}

	for host, proc := range p.procs {
		if _, ok := desired[host]; ok {
			continue
		}
		_ = stopProcess(proc)
		delete(p.procs, host)
	}

	published := make([]string, 0, len(hosts))
	collided := make([]string, 0)

	for _, host := range hosts {
		if host == "" {
			continue
		}
		if proc, ok := p.procs[host]; ok {
			if proc.cmd.ProcessState == nil || !proc.cmd.ProcessState.Exited() {
				published = append(published, host)
				continue
			}
			delete(p.procs, host)
		}

		proc, err := startPublisher(host, p.address)
		if err != nil {
			return nil, nil, fmt.Errorf("start %s: %w", host, err)
		}

		select {
		case err := <-proc.waitCh:
			if err != nil {
				log.Printf("publisher for %s exited early: %s", host, strings.TrimSpace(proc.output.String()))
			}
			collided = append(collided, host)
		case <-time.After(publishStartTimeout):
			p.procs[host] = proc
			published = append(published, host)
		}
	}

	sort.Strings(published)
	sort.Strings(collided)
	return published, collided, nil
}

func (p *avahiPublisher) reapExited() {
	for host, proc := range p.procs {
		select {
		case err := <-proc.waitCh:
			if err != nil {
				log.Printf("publisher for %s exited: %s", host, strings.TrimSpace(proc.output.String()))
			}
			delete(p.procs, host)
		default:
		}
	}
}

func startPublisher(host, address string) (*publishedProcess, error) {
	// Do not publish reverse PTR records for aliases. The node's primary
	// hostname already owns the reverse mapping for its LAN IP.
	cmd := exec.Command("avahi-publish-address", "-R", host, address)
	cmd.Env = os.Environ()
	output := &bytes.Buffer{}
	cmd.Stdout = output
	cmd.Stderr = output
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	proc := &publishedProcess{
		cmd:    cmd,
		host:   host,
		output: output,
		waitCh: make(chan error, 1),
	}
	go func() {
		proc.waitCh <- cmd.Wait()
	}()
	return proc, nil
}

func stopProcess(proc *publishedProcess) error {
	if proc == nil || proc.cmd == nil || proc.cmd.Process == nil {
		return nil
	}
	if proc.cmd.ProcessState != nil && proc.cmd.ProcessState.Exited() {
		return nil
	}
	_ = proc.cmd.Process.Signal(os.Interrupt)
	select {
	case <-proc.waitCh:
		return nil
	case <-time.After(2 * time.Second):
		_ = proc.cmd.Process.Kill()
		<-proc.waitCh
		return nil
	}
}

func (p *avahiPublisher) Close() error {
	for host, proc := range p.procs {
		if err := stopProcess(proc); err != nil {
			return err
		}
		delete(p.procs, host)
	}
	return nil
}

func serveHTTP(addr string, state *controllerState) {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !state.ready() {
			http.Error(w, state.lastErrorText(), http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready\n"))
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = w.Write([]byte(state.metrics()))
	})
	mux.HandleFunc("/state", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(state.snapshot())
	})

	server := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("http server: %v", err)
	}
}

func (s *controllerState) markDBusError(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dbusConnected = false
	s.lastError = err.Error()
}

func (s *controllerState) markReconcileError(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.initialSyncDone = true
	s.lastError = err.Error()
	s.lastReconcile = time.Now().UTC()
	s.reconcileErrorTotal++
}

func (s *controllerState) markReconcileSuccess(desired []string, published []string, collided []string, desiredHash string) {
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.initialSyncDone = true
	s.lastError = ""
	s.lastReconcile = now
	s.lastSuccess = now
	s.desiredHosts = append([]string(nil), desired...)
	s.desiredHash = desiredHash
	s.publishedHosts = append([]string(nil), published...)
	s.publishedHash = hashHosts(published)
	s.collidedHosts = append([]string(nil), collided...)
	accounted := append([]string(nil), published...)
	accounted = append(accounted, collided...)
	sort.Strings(accounted)
	s.accountedHash = hashHosts(accounted)
	s.reconcileOKTotal++
}

func (s *controllerState) setDesired(hosts []string, hash string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.initialSyncDone = true
	s.desiredHosts = append([]string(nil), hosts...)
	s.desiredHash = hash
	s.lastReconcile = time.Now().UTC()
}

func (s *controllerState) setDBusConnected(connected bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dbusConnected = connected
}

func (s *controllerState) getHashes() (string, string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.publishedHash, s.accountedHash
}

func (s *controllerState) getPublishedHosts() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]string(nil), s.publishedHosts...)
}

func (s *controllerState) getCollidedHosts() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]string(nil), s.collidedHosts...)
}

// ready reports whether the mDNS service is operational — i.e. D-Bus is connected
// and we have a non-empty published state that is not stale. It does NOT require
// the K8s API to be reachable; K8s API errors cause reconcile failures but the
// previously published mDNS aliases remain active and the service continues to work.
// This prevents the K8s readinessProbe from blocking on transient API outages
// (e.g. etcd connectivity issues on a remote control-plane node).
func (s *controllerState) ready() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if !s.dbusConnected {
		return false
	}
	// Must have successfully published at least one alias.
	if len(s.publishedHosts) == 0 {
		return false
	}
	// Published state must not be stale.
	if s.lastSuccess.IsZero() || time.Since(s.lastSuccess) > s.staleAfter {
		return false
	}
	return true
}

func (s *controllerState) lastErrorText() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.lastError == "" {
		return "not ready"
	}
	return s.lastError
}

func (s *controllerState) metrics() string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	readyValue := 0
	if s.initialSyncDone && s.dbusConnected && s.desiredHash != "" && s.desiredHash == s.accountedHash && !s.lastSuccess.IsZero() && time.Since(s.lastSuccess) <= s.staleAfter && (len(s.desiredHosts) == 0 || len(s.publishedHosts) > 0) {
		readyValue = 1
	}

	lastSuccess := float64(0)
	if !s.lastSuccess.IsZero() {
		lastSuccess = float64(s.lastSuccess.Unix())
	}

	return fmt.Sprintf(
		"mdns_ready %d\nmdns_desired_hosts %d\nmdns_published_hosts %d\nmdns_collided_hosts %d\nmdns_reconcile_success_total %d\nmdns_reconcile_error_total %d\nmdns_last_successful_reconcile_timestamp_seconds %.0f\n",
		readyValue,
		len(s.desiredHosts),
		len(s.publishedHosts),
		len(s.collidedHosts),
		s.reconcileOKTotal,
		s.reconcileErrorTotal,
		lastSuccess,
	)
}

func (s *controllerState) snapshot() map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return map[string]any{
		"dbus_connected":        s.dbusConnected,
		"desired_hash":          s.desiredHash,
		"desired_hosts":         append([]string(nil), s.desiredHosts...),
		"initial_sync_done":     s.initialSyncDone,
		"last_error":            s.lastError,
		"last_reconcile":        s.lastReconcile,
		"last_success":          s.lastSuccess,
		"collided_hosts":        append([]string(nil), s.collidedHosts...),
		"published_hash":        s.publishedHash,
		"published_hosts":       append([]string(nil), s.publishedHosts...),
		"reconcile_error_total": s.reconcileErrorTotal,
		"reconcile_ok_total":    s.reconcileOKTotal,
		"ready":                 s.ready(),
		"stale_after":           s.staleAfter.String(),
	}
}

func hashHosts(hosts []string) string {
	sum := sha256.Sum256([]byte(strings.Join(hosts, "\n")))
	return hex.EncodeToString(sum[:])
}

func sameStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func parseDurationEnv(key string, fallback time.Duration) (time.Duration, error) {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback, nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", key, err)
	}
	return duration, nil
}

func parseBoolEnv(key string, fallback bool) (bool, error) {
	value := strings.TrimSpace(strings.ToLower(os.Getenv(key)))
	if value == "" {
		return fallback, nil
	}
	switch value {
	case "true", "1", "yes", "on", "enable", "enabled":
		return true, nil
	case "false", "0", "no", "off", "disable", "disabled":
		return false, nil
	default:
		return false, fmt.Errorf("parse %s: invalid boolean %q", key, value)
	}
}

func getenvDefault(key, fallback string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	return value
}

func detectInterface() (string, error) {
	conn, err := net.Dial("udp4", "8.8.8.8:53")
	if err != nil {
		return "", err
	}
	local := conn.LocalAddr().(*net.UDPAddr)
	_ = conn.Close()

	interfaces, err := net.Interfaces()
	if err != nil {
		return "", err
	}
	for _, iface := range interfaces {
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if ok && ipNet.IP.Equal(local.IP) {
				return iface.Name, nil
			}
		}
	}
	return "", errors.New("could not match local ip to interface")
}

func detectAddress(interfaceName string) (string, error) {
	iface, err := net.InterfaceByName(interfaceName)
	if err != nil {
		return "", err
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return "", err
	}
	for _, addr := range addrs {
		ipNet, ok := addr.(*net.IPNet)
		if !ok {
			continue
		}
		if ip := ipNet.IP.To4(); ip != nil && !ip.IsLoopback() {
			return ip.String(), nil
		}
	}
	return "", fmt.Errorf("no ipv4 address found on %s", interfaceName)
}
