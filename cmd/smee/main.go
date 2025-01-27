package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"net/url"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/go-logr/logr"
	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/dhcpv4/server4"
	"github.com/tinkerbell/ipxedust"
	"github.com/tinkerbell/ipxedust/ihttp"
	"github.com/tinkerbell/smee/internal/dhcp/handler"
	"github.com/tinkerbell/smee/internal/dhcp/handler/proxy"
	"github.com/tinkerbell/smee/internal/dhcp/handler/reservation"
	"github.com/tinkerbell/smee/internal/dhcp/server"
	"github.com/tinkerbell/smee/internal/ipxe/http"
	"github.com/tinkerbell/smee/internal/ipxe/script"
	"github.com/tinkerbell/smee/internal/iso"
	"github.com/tinkerbell/smee/internal/metric"
	"github.com/tinkerbell/smee/internal/otel"
	"github.com/tinkerbell/smee/internal/syslog"
	"golang.org/x/sync/errgroup"
)

var (
	// GitRev is the git revision of the build. It is set by the Makefile.
	GitRev = "unknown (use make)"

	startTime = time.Now()
)

const (
	name                         = "smee"
	dhcpModeProxy       dhcpMode = "proxy"
	dhcpModeReservation dhcpMode = "reservation"
	dhcpModeAutoProxy   dhcpMode = "auto-proxy"
	// magicString comes from the HookOS repo
	// ref: https://github.com/tinkerbell/hook/blob/main/linuxkit-templates/hook.template.yaml
	magicString = `464vn90e7rbj08xbwdjejmdf4it17c5zfzjyfhthbh19eij201hjgit021bmpdb9ctrc87x2ymc8e7icu4ffi15x1hah9iyaiz38ckyap8hwx2vt5rm44ixv4hau8iw718q5yd019um5dt2xpqqa2rjtdypzr5v1gun8un110hhwp8cex7pqrh2ivh0ynpm4zkkwc8wcn367zyethzy7q8hzudyeyzx3cgmxqbkh825gcak7kxzjbgjajwizryv7ec1xm2h0hh7pz29qmvtgfjj1vphpgq1zcbiiehv52wrjy9yq473d9t1rvryy6929nk435hfx55du3ih05kn5tju3vijreru1p6knc988d4gfdz28eragvryq5x8aibe5trxd0t6t7jwxkde34v6pj1khmp50k6qqj3nzgcfzabtgqkmeqhdedbvwf3byfdma4nkv3rcxugaj2d0ru30pa2fqadjqrtjnv8bu52xzxv7irbhyvygygxu1nt5z4fh9w1vwbdcmagep26d298zknykf2e88kumt59ab7nq79d8amnhhvbexgh48e8qc61vq2e9qkihzt1twk1ijfgw70nwizai15iqyted2dt9gfmf2gg7amzufre79hwqkddc1cd935ywacnkrnak6r7xzcz7zbmq3kt04u2hg1iuupid8rt4nyrju51e6uejb2ruu36g9aibmz3hnmvazptu8x5tyxk820g2cdpxjdij766bt2n3djur7v623a2v44juyfgz80ekgfb9hkibpxh3zgknw8a34t4jifhf116x15cei9hwch0fye3xyq0acuym8uhitu5evc4rag3ui0fny3qg4kju7zkfyy8hwh537urd5uixkzwu5bdvafz4jmv7imypj543xg5em8jk8cgk7c4504xdd5e4e71ihaumt6u5u2t1w7um92fepzae8p0vq93wdrd1756npu1pziiur1payc7kmdwyxg3hj5n4phxbc29x0tcddamjrwt260b0w`
)

type config struct {
	syslog         syslogConfig
	tftp           tftp
	ipxeHTTPBinary ipxeHTTPBinary
	ipxeHTTPScript ipxeHTTPScript
	dhcp           dhcpConfig
	iso            isoConfig

	// loglevel is the log level for smee.
	logLevel string
	backends dhcpBackends
	otel     otelConfig
}

type syslogConfig struct {
	enabled  bool
	bindAddr string
	bindPort int
}

type tftp struct {
	bindAddr        string
	bindPort        int
	blockSize       int
	enabled         bool
	ipxeScriptPatch string
	timeout         time.Duration
}

type ipxeHTTPBinary struct {
	enabled bool
}

type ipxeHTTPScript struct {
	enabled               bool
	bindAddr              string
	bindPort              int
	extraKernelArgs       string
	hookURL               string
	tinkServer            string
	tinkServerUseTLS      bool
	tinkServerInsecureTLS bool
	trustedProxies        string
	retries               int
	retryDelay            int
}

type dhcpMode string

type dhcpConfig struct {
	enabled           bool
	mode              string
	bindAddr          string
	bindInterface     string
	ipForPacket       string
	syslogIP          string
	tftpIP            string
	tftpPort          int
	httpIpxeBinaryURL urlBuilder
	httpIpxeScript    httpIpxeScript
	httpIpxeScriptURL string
}

type urlBuilder struct {
	Scheme string
	Host   string
	Port   int
	Path   string
}

type httpIpxeScript struct {
	urlBuilder
	// injectMacAddress will prepend the hardware mac address to the ipxe script URL file name.
	// For example: http://1.2.3.4/my/loc/auto.ipxe -> http://1.2.3.4/my/loc/40:15:ff:89:cc:0e/auto.ipxe
	// Setting this to false is useful when you are not using the auto.ipxe script in Smee.
	injectMacAddress bool
}

type dhcpBackends struct {
	file       File
	kubernetes Kube
	Noop       Noop
}

type otelConfig struct {
	endpoint string
	insecure bool
}

type isoConfig struct {
	enabled           bool
	url               string
	magicString       string
	staticIPAMEnabled bool
}

func main() {
	cfg := &config{}
	cli := newCLI(cfg, flag.NewFlagSet(name, flag.ExitOnError))
	_ = cli.Parse(os.Args[1:])

	log := defaultLogger(cfg.logLevel)
	log.Info("starting", "version", GitRev)

	ctx, done := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGHUP, syscall.SIGTERM)
	defer done()
	oCfg := otel.Config{
		Servicename: "smee",
		Endpoint:    cfg.otel.endpoint,
		Insecure:    cfg.otel.insecure,
		Logger:      log,
	}
	ctx, otelShutdown, err := otel.Init(ctx, oCfg)
	if err != nil {
		log.Error(err, "failed to initialize OpenTelemetry")
		panic(err)
	}
	defer otelShutdown()
	metric.Init()

	g, ctx := errgroup.WithContext(ctx)
	// syslog
	if cfg.syslog.enabled {
		addr := fmt.Sprintf("%s:%d", cfg.syslog.bindAddr, cfg.syslog.bindPort)
		log.Info("starting syslog server", "bind_addr", addr)
		g.Go(func() error {
			if err := syslog.StartReceiver(ctx, log, addr, 1); err != nil {
				log.Error(err, "syslog server failure")
				return err
			}
			<-ctx.Done()
			log.Info("syslog server stopped")
			return nil
		})
	}

	// tftp
	if cfg.tftp.enabled {
		tftpServer := &ipxedust.Server{
			Log:                  log.WithValues("service", "github.com/tinkerbell/smee").WithName("github.com/tinkerbell/ipxedust"),
			HTTP:                 ipxedust.ServerSpec{Disabled: true}, // disabled because below we use the http handlerfunc instead.
			EnableTFTPSinglePort: true,
		}
		tftpServer.EnableTFTPSinglePort = true
		addr := fmt.Sprintf("%s:%d", cfg.tftp.bindAddr, cfg.tftp.bindPort)
		if ip, err := netip.ParseAddrPort(addr); err == nil {
			tftpServer.TFTP = ipxedust.ServerSpec{
				Disabled:  false,
				Addr:      ip,
				Timeout:   cfg.tftp.timeout,
				Patch:     []byte(cfg.tftp.ipxeScriptPatch),
				BlockSize: cfg.tftp.blockSize,
			}
			// start the ipxe binary tftp server
			log.Info("starting tftp server", "bind_addr", addr)
			g.Go(func() error {
				return tftpServer.ListenAndServe(ctx)
			})
		} else {
			log.Error(err, "invalid bind address")
			panic(fmt.Errorf("invalid bind address: %w", err))
		}
	}

	handlers := http.HandlerMapping{}
	// http ipxe binaries
	if cfg.ipxeHTTPBinary.enabled {
		// serve ipxe binaries from the "/ipxe/" URI.
		handlers["/ipxe/"] = ihttp.Handler{
			Log:   log.WithValues("service", "github.com/tinkerbell/smee").WithName("github.com/tinkerbell/ipxedust"),
			Patch: []byte(cfg.tftp.ipxeScriptPatch),
		}.Handle
	}

	// http ipxe script
	if cfg.ipxeHTTPScript.enabled {
		br, err := cfg.backend(ctx, log)
		if err != nil {
			panic(fmt.Errorf("failed to create backend: %w", err))
		}
		jh := script.Handler{
			Logger:                log,
			Backend:               br,
			OSIEURL:               cfg.ipxeHTTPScript.hookURL,
			ExtraKernelParams:     strings.Split(cfg.ipxeHTTPScript.extraKernelArgs, " "),
			PublicSyslogFQDN:      cfg.dhcp.syslogIP,
			TinkServerTLS:         cfg.ipxeHTTPScript.tinkServerUseTLS,
			TinkServerInsecureTLS: cfg.ipxeHTTPScript.tinkServerInsecureTLS,
			TinkServerGRPCAddr:    cfg.ipxeHTTPScript.tinkServer,
			IPXEScriptRetries:     cfg.ipxeHTTPScript.retries,
			IPXEScriptRetryDelay:  cfg.ipxeHTTPScript.retryDelay,
			StaticIPXEEnabled:     (dhcpMode(cfg.dhcp.mode) == dhcpModeAutoProxy),
		}

		// serve ipxe script from the "/" URI.
		handlers["/"] = jh.HandlerFunc()
	}

	if cfg.iso.enabled {
		br, err := cfg.backend(ctx, log)
		if err != nil {
			panic(fmt.Errorf("failed to create backend: %w", err))
		}
		ih := iso.Handler{
			Logger:             log,
			Backend:            br,
			SourceISO:          cfg.iso.url,
			ExtraKernelParams:  strings.Split(cfg.ipxeHTTPScript.extraKernelArgs, " "),
			Syslog:             cfg.dhcp.syslogIP,
			TinkServerTLS:      cfg.ipxeHTTPScript.tinkServerUseTLS,
			TinkServerGRPCAddr: cfg.ipxeHTTPScript.tinkServer,
			StaticIPAMEnabled:  cfg.iso.staticIPAMEnabled,
			MagicString: func() string {
				if cfg.iso.magicString == "" {
					return magicString
				}
				return cfg.iso.magicString
			}(),
		}
		isoHandler, err := ih.HandlerFunc()
		if err != nil {
			panic(fmt.Errorf("failed to create iso handler: %w", err))
		}
		handlers["/iso/"] = isoHandler
	}

	if len(handlers) > 0 {
		// start the http server for ipxe binaries and scripts
		tp := parseTrustedProxies(cfg.ipxeHTTPScript.trustedProxies)
		httpServer := &http.Config{
			GitRev:         GitRev,
			StartTime:      startTime,
			Logger:         log,
			TrustedProxies: tp,
		}
		bindAddr := fmt.Sprintf("%s:%d", cfg.ipxeHTTPScript.bindAddr, cfg.ipxeHTTPScript.bindPort)
		log.Info("serving http", "addr", bindAddr, "trusted_proxies", tp)
		g.Go(func() error {
			return httpServer.ServeHTTP(ctx, bindAddr, handlers)
		})
	}

	// dhcp serving
	if cfg.dhcp.enabled {
		dh, err := cfg.dhcpHandler(ctx, log)
		if err != nil {
			log.Error(err, "failed to create dhcp listener")
			panic(fmt.Errorf("failed to create dhcp listener: %w", err))
		}
		log.Info("starting dhcp server", "bind_addr", cfg.dhcp.bindAddr)
		g.Go(func() error {
			bindAddr, err := netip.ParseAddrPort(cfg.dhcp.bindAddr)
			if err != nil {
				panic(fmt.Errorf("invalid tftp address for DHCP server: %w", err))
			}
			conn, err := server4.NewIPv4UDPConn(cfg.dhcp.bindInterface, net.UDPAddrFromAddrPort(bindAddr))
			if err != nil {
				panic(err)
			}
			defer conn.Close()
			ds := &server.DHCP{Logger: log, Conn: conn, Handlers: []server.Handler{dh}}

			return ds.Serve(ctx)
		})
	}

	if err := g.Wait(); err != nil && !errors.Is(err, context.Canceled) {
		log.Error(err, "failed running all Smee services")
		panic(err)
	}
	log.Info("smee is shutting down")
}

func numTrue(b ...bool) int {
	n := 0
	for _, v := range b {
		if v {
			n++
		}
	}
	return n
}

func (c *config) backend(ctx context.Context, log logr.Logger) (handler.BackendReader, error) {
	if c.backends.file.Enabled || c.backends.Noop.Enabled {
		// the kubernetes backend is enabled by default so we disable it
		// if another backend is enabled so that users don't have to explicitly
		// set the CLI flag to disable it when using another backend.
		c.backends.kubernetes.Enabled = false
	}
	var be handler.BackendReader
	switch {
	case numTrue(c.backends.file.Enabled, c.backends.kubernetes.Enabled, c.backends.Noop.Enabled) > 1:
		return nil, errors.New("only one backend can be enabled at a time")
	case c.backends.Noop.Enabled:
		if c.dhcp.mode != string(dhcpModeAutoProxy) {
			return nil, errors.New("noop backend can only be used with --dhcp-mode=auto-proxy")
		}
		be = c.backends.Noop.backend()
	case c.backends.file.Enabled:
		b, err := c.backends.file.backend(ctx, log)
		if err != nil {
			return nil, fmt.Errorf("failed to create file backend: %w", err)
		}
		be = b
	default: // default backend is kubernetes
		b, err := c.backends.kubernetes.backend(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to create kubernetes backend: %w", err)
		}
		be = b
	}

	return be, nil
}

func (c *config) dhcpHandler(ctx context.Context, log logr.Logger) (server.Handler, error) {
	// 1. create the handler
	// 2. create the backend
	// 3. add the backend to the handler
	pktIP, err := netip.ParseAddr(c.dhcp.ipForPacket)
	if err != nil {
		return nil, fmt.Errorf("invalid bind address: %w", err)
	}
	tftpIP, err := netip.ParseAddrPort(fmt.Sprintf("%s:%d", c.dhcp.tftpIP, c.dhcp.tftpPort))
	if err != nil {
		return nil, fmt.Errorf("invalid tftp address for DHCP server: %w", err)
	}
	httpBinaryURL := &url.URL{
		Scheme: c.dhcp.httpIpxeBinaryURL.Scheme,
		Host:   fmt.Sprintf("%s:%d", c.dhcp.httpIpxeBinaryURL.Host, c.dhcp.httpIpxeBinaryURL.Port),
		Path:   c.dhcp.httpIpxeBinaryURL.Path,
	}
	if _, err := url.Parse(httpBinaryURL.String()); err != nil {
		return nil, fmt.Errorf("invalid http ipxe binary url: %w", err)
	}

	var httpScriptURL *url.URL
	if c.dhcp.httpIpxeScriptURL != "" {
		httpScriptURL, err = url.Parse(c.dhcp.httpIpxeScriptURL)
		if err != nil {
			return nil, fmt.Errorf("invalid http ipxe script url: %w", err)
		}
	} else {
		httpScriptURL = &url.URL{
			Scheme: c.dhcp.httpIpxeScript.Scheme,
			Host: func() string {
				switch c.dhcp.httpIpxeScript.Scheme {
				case "http":
					if c.dhcp.httpIpxeScript.Port == 80 {
						return c.dhcp.httpIpxeScript.Host
					}
				case "https":
					if c.dhcp.httpIpxeScript.Port == 443 {
						return c.dhcp.httpIpxeScript.Host
					}
				}
				return fmt.Sprintf("%s:%d", c.dhcp.httpIpxeScript.Host, c.dhcp.httpIpxeScript.Port)
			}(),
			Path: c.dhcp.httpIpxeScript.Path,
		}
	}

	if _, err := url.Parse(httpScriptURL.String()); err != nil {
		return nil, fmt.Errorf("invalid http ipxe script url: %w", err)
	}
	ipxeScript := func(*dhcpv4.DHCPv4) *url.URL {
		return httpScriptURL
	}
	if c.dhcp.httpIpxeScript.injectMacAddress {
		ipxeScript = func(d *dhcpv4.DHCPv4) *url.URL {
			u := *httpScriptURL
			p := path.Base(u.Path)
			u.Path = path.Join(path.Dir(u.Path), d.ClientHWAddr.String(), p)
			return &u
		}
	}
	backend, err := c.backend(ctx, log)
	if err != nil {
		return nil, fmt.Errorf("failed to create backend: %w", err)
	}

	switch dhcpMode(c.dhcp.mode) {
	case dhcpModeReservation:
		syslogIP, err := netip.ParseAddr(c.dhcp.syslogIP)
		if err != nil {
			return nil, fmt.Errorf("invalid syslog address: %w", err)
		}
		dh := &reservation.Handler{
			Backend: backend,
			IPAddr:  pktIP,
			Log:     log,
			Netboot: reservation.Netboot{
				IPXEBinServerTFTP: tftpIP,
				IPXEBinServerHTTP: httpBinaryURL,
				IPXEScriptURL:     ipxeScript,
				Enabled:           true,
			},
			OTELEnabled: true,
			SyslogAddr:  syslogIP,
		}
		return dh, nil
	case dhcpModeProxy:
		dh := &proxy.Handler{
			Backend: backend,
			IPAddr:  pktIP,
			Log:     log,
			Netboot: proxy.Netboot{
				IPXEBinServerTFTP: tftpIP,
				IPXEBinServerHTTP: httpBinaryURL,
				IPXEScriptURL:     ipxeScript,
				Enabled:           true,
			},
			OTELEnabled:      true,
			AutoProxyEnabled: false,
		}
		return dh, nil
	case dhcpModeAutoProxy:
		dh := &proxy.Handler{
			Backend: backend,
			IPAddr:  pktIP,
			Log:     log,
			Netboot: proxy.Netboot{
				IPXEBinServerTFTP: tftpIP,
				IPXEBinServerHTTP: httpBinaryURL,
				IPXEScriptURL:     ipxeScript,
				Enabled:           true,
			},
			OTELEnabled:      true,
			AutoProxyEnabled: true,
		}
		return dh, nil
	}

	return nil, errors.New("invalid dhcp mode")
}

// defaultLogger uses the slog logr implementation.
func defaultLogger(level string) logr.Logger {
	// source file and function can be long. This makes the logs less readable.
	// truncate source file and function to last 3 parts for improved readability.
	customAttr := func(_ []string, a slog.Attr) slog.Attr {
		if a.Key == slog.SourceKey {
			ss, ok := a.Value.Any().(*slog.Source)
			if !ok || ss == nil {
				return a
			}
			f := strings.Split(ss.Function, "/")
			if len(f) > 3 {
				ss.Function = filepath.Join(f[len(f)-3:]...)
			}
			p := strings.Split(ss.File, "/")
			if len(p) > 3 {
				ss.File = filepath.Join(p[len(p)-3:]...)
			}

			return a
		}

		return a
	}
	opts := &slog.HandlerOptions{AddSource: true, ReplaceAttr: customAttr}
	switch level {
	case "debug":
		opts.Level = slog.LevelDebug
	default:
		opts.Level = slog.LevelInfo
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, opts))

	return logr.FromSlogHandler(log.Handler())
}

func parseTrustedProxies(trustedProxies string) (result []string) {
	for _, cidr := range strings.Split(trustedProxies, ",") {
		cidr = strings.TrimSpace(cidr)
		if cidr == "" {
			continue
		}
		_, _, err := net.ParseCIDR(cidr)
		if err != nil {
			// Its not a cidr, but maybe its an IP
			if ip := net.ParseIP(cidr); ip != nil {
				if ip.To4() != nil {
					cidr += "/32"
				} else {
					cidr += "/128"
				}
			} else {
				// not an IP, panic
				panic("invalid ip cidr in TRUSTED_PROXIES cidr=" + cidr)
			}
		}
		result = append(result, cidr)
	}

	return result
}

func (d dhcpMode) String() string {
	return string(d)
}
