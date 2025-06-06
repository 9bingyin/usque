package cmd

import (
	"context"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"time"

	"github.com/Diniboy1123/usque/api"
	"github.com/Diniboy1123/usque/config"
	"github.com/Diniboy1123/usque/internal"
	"github.com/spf13/cobra"
	"golang.zx2c4.com/wireguard/tun/netstack"
)

var httpProxyCmd = &cobra.Command{
	Use:   "http-proxy",
	Short: "Expose Warp as an HTTP proxy with CONNECT support",
	Long:  "Dual-stack HTTP proxy with CONNECT support. Doesn't require elevated privileges.",
	Run: func(cmd *cobra.Command, args []string) {
		if !config.ConfigLoaded {
			cmd.Println("Config not loaded. Please register first.")
			return
		}

		sni, err := cmd.Flags().GetString("sni-address")
		if err != nil {
			cmd.Printf("Failed to get SNI address: %v\n", err)
			return
		}

		privKey, err := config.AppConfig.GetEcPrivateKey()
		if err != nil {
			cmd.Printf("Failed to get private key: %v\n", err)
			return
		}
		peerPubKey, err := config.AppConfig.GetEcEndpointPublicKey()
		if err != nil {
			cmd.Printf("Failed to get public key: %v\n", err)
			return
		}

		cert, err := internal.GenerateCert(privKey, &privKey.PublicKey)
		if err != nil {
			cmd.Printf("Failed to generate cert: %v\n", err)
			return
		}

		tlsConfig, err := api.PrepareTlsConfig(privKey, peerPubKey, cert, sni)
		if err != nil {
			cmd.Printf("Failed to prepare TLS config: %v\n", err)
			return
		}

		keepalivePeriod, err := cmd.Flags().GetDuration("keepalive-period")
		if err != nil {
			cmd.Printf("Failed to get keepalive period: %v\n", err)
			return
		}
		initialPacketSize, err := cmd.Flags().GetUint16("initial-packet-size")
		if err != nil {
			cmd.Printf("Failed to get initial packet size: %v\n", err)
			return
		}

		bindAddress, err := cmd.Flags().GetString("bind")
		if err != nil {
			cmd.Printf("Failed to get bind address: %v\n", err)
			return
		}

		port, err := cmd.Flags().GetString("port")
		if err != nil {
			cmd.Printf("Failed to get port: %v\n", err)
			return
		}

		connectPort, err := cmd.Flags().GetInt("connect-port")
		if err != nil {
			cmd.Printf("Failed to get connect port: %v\n", err)
			return
		}

		var endpoint *net.UDPAddr
		if ipv6, err := cmd.Flags().GetBool("ipv6"); err == nil && !ipv6 {
			endpoint = &net.UDPAddr{
				IP:   net.ParseIP(config.AppConfig.EndpointV4),
				Port: connectPort,
			}
		} else {
			endpoint = &net.UDPAddr{
				IP:   net.ParseIP(config.AppConfig.EndpointV6),
				Port: connectPort,
			}
		}

		tunnelIPv4, err := cmd.Flags().GetBool("no-tunnel-ipv4")
		if err != nil {
			cmd.Printf("Failed to get no tunnel IPv4: %v\n", err)
			return
		}

		tunnelIPv6, err := cmd.Flags().GetBool("no-tunnel-ipv6")
		if err != nil {
			cmd.Printf("Failed to get no tunnel IPv6: %v\n", err)
			return
		}

		var localAddresses []netip.Addr
		if !tunnelIPv4 {
			v4, err := netip.ParseAddr(config.AppConfig.IPv4)
			if err != nil {
				cmd.Printf("Failed to parse IPv4 address: %v\n", err)
				return
			}
			localAddresses = append(localAddresses, v4)
		}
		if !tunnelIPv6 {
			v6, err := netip.ParseAddr(config.AppConfig.IPv6)
			if err != nil {
				cmd.Printf("Failed to parse IPv6 address: %v\n", err)
				return
			}
			localAddresses = append(localAddresses, v6)
		}

		dnsServers, err := cmd.Flags().GetStringArray("dns")
		if err != nil {
			cmd.Printf("Failed to get DNS servers: %v\n", err)
			return
		}

		var dnsAddrs []netip.Addr
		for _, dns := range dnsServers {
			addr, err := netip.ParseAddr(dns)
			if err != nil {
				cmd.Printf("Failed to parse DNS server: %v\n", err)
				return
			}
			dnsAddrs = append(dnsAddrs, addr)
		}

		mtu, err := cmd.Flags().GetInt("mtu")
		if err != nil {
			cmd.Printf("Failed to get MTU: %v\n", err)
			return
		}
		if mtu != 1280 {
			log.Println("Warning: MTU is not the default 1280. This is not supported. Packet loss and other issues may occur.")
		}

		var username string
		var password string
		if u, err := cmd.Flags().GetString("username"); err == nil && u != "" {
			username = u
		}
		if p, err := cmd.Flags().GetString("password"); err == nil && p != "" {
			password = p
		}

		reconnectDelay, err := cmd.Flags().GetDuration("reconnect-delay")
		if err != nil {
			cmd.Printf("Failed to get reconnect delay: %v\n", err)
			return
		}

		var authHeader string
		if username != "" && password != "" {
			authHeader = "Basic " + internal.LoginToBase64(username, password)
		}

		tunDev, tunNet, err := netstack.CreateNetTUN(localAddresses, dnsAddrs, mtu)
		if err != nil {
			cmd.Printf("Failed to create virtual TUN device: %v\n", err)
			return
		}
		defer tunDev.Close()

		go api.MaintainTunnel(context.Background(), tlsConfig, keepalivePeriod, initialPacketSize, endpoint, api.NewNetstackAdapter(tunDev), mtu, reconnectDelay)

		server := &http.Server{
			Addr: net.JoinHostPort(bindAddress, port),
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !authenticate(r, authHeader) {
					w.Header().Set("Proxy-Authenticate", `Basic realm="Proxy"`)
					http.Error(w, "Proxy authentication required", http.StatusProxyAuthRequired)
					return
				}

				if r.Method == http.MethodConnect {
					handleHTTPSConnect(w, r, tunNet)
				} else {
					handleHTTPProxy(w, r, tunNet)
				}
			}),
		}

		log.Printf("HTTP proxy listening on %s:%s\n", bindAddress, port)
		if err := server.ListenAndServe(); err != nil {
			cmd.Printf("Failed to start HTTP proxy: %v\n", err)
		}
	},
}

// authenticate verifies the Proxy-Authorization header in an HTTP request.
//
// Parameters:
//   - r: *http.Request - The incoming HTTP request.
//   - expectedAuth: string - The expected authorization token.
//
// Returns:
//   - bool: True if the authorization header matches the expected value, otherwise false.
func authenticate(r *http.Request, expectedAuth string) bool {
	authHeader := r.Header.Get("Proxy-Authorization")
	return authHeader == expectedAuth
}

// handleHTTPSConnect processes HTTPS CONNECT proxy requests, establishing a tunnel to the destination.
//
// Parameters:
//   - w: http.ResponseWriter - The HTTP response writer.
//   - r: *http.Request - The incoming HTTP CONNECT request.
//   - tunNet: *netstack.Net - The network stack used for dialing the destination.
func handleHTTPSConnect(w http.ResponseWriter, r *http.Request, tunNet *netstack.Net) {
	destConn, err := tunNet.DialContext(r.Context(), "tcp", r.Host)
	if err != nil {
		http.Error(w, "Unable to connect to destination", http.StatusServiceUnavailable)
		return
	}
	defer destConn.Close()

	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "Hijacking not supported", http.StatusInternalServerError)
		return
	}

	clientConn, _, err := hj.Hijack()
	if err != nil {
		http.Error(w, "Hijacking failed", http.StatusInternalServerError)
		return
	}
	defer clientConn.Close()

	clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))

	go io.Copy(destConn, clientConn)
	io.Copy(clientConn, destConn)
}

// handleHTTPProxy forwards HTTP proxy requests to the destination and relays responses back to the client.
//
// Parameters:
//   - w: http.ResponseWriter - The HTTP response writer.
//   - r: *http.Request - The incoming HTTP request.
//   - tunNet: *netstack.Net - The network stack used for making outbound requests.
func handleHTTPProxy(w http.ResponseWriter, r *http.Request, tunNet *netstack.Net) {
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: tunNet.DialContext,
		},
	}

	req, err := http.NewRequest(r.Method, r.URL.String(), r.Body)
	if err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}

	req.Header = r.Header

	resp, err := client.Do(req)
	if err != nil {
		http.Error(w, "Failed to reach destination", http.StatusServiceUnavailable)
		return
	}
	defer resp.Body.Close()

	copyHeader(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// copyHeader copies HTTP headers from one header map to another.
//
// Parameters:
//   - dst: http.Header - The destination header map.
//   - src: http.Header - The source header map.
func copyHeader(dst, src http.Header) {
	for k, vv := range src {
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

func init() {
	httpProxyCmd.Flags().StringP("bind", "b", "0.0.0.0", "Address to bind the HTTP proxy to")
	httpProxyCmd.Flags().StringP("port", "p", "8000", "Port to listen on for HTTP proxy")
	httpProxyCmd.Flags().StringP("username", "u", "", "Username for proxy authentication (specify both username and password to enable)")
	httpProxyCmd.Flags().StringP("password", "w", "", "Password for proxy authentication (specify both username and password to enable)")
	httpProxyCmd.Flags().IntP("connect-port", "P", 443, "Used port for MASQUE connection")
	httpProxyCmd.Flags().StringArrayP("dns", "d", []string{"9.9.9.9", "149.112.112.112", "2620:fe::fe", "2620:fe::9"}, "DNS servers to use inside the MASQUE tunnel")
	httpProxyCmd.Flags().BoolP("ipv6", "6", false, "Use IPv6 for MASQUE connection")
	httpProxyCmd.Flags().BoolP("no-tunnel-ipv4", "F", false, "Disable IPv4 inside the MASQUE tunnel")
	httpProxyCmd.Flags().BoolP("no-tunnel-ipv6", "S", false, "Disable IPv6 inside the MASQUE tunnel")
	httpProxyCmd.Flags().StringP("sni-address", "s", internal.ConnectSNI, "SNI address to use for MASQUE connection")
	httpProxyCmd.Flags().DurationP("keepalive-period", "k", 30*time.Second, "Keepalive period for MASQUE connection")
	httpProxyCmd.Flags().IntP("mtu", "m", 1280, "MTU for MASQUE connection")
	httpProxyCmd.Flags().Uint16P("initial-packet-size", "i", 1242, "Initial packet size for MASQUE connection")
	httpProxyCmd.Flags().DurationP("reconnect-delay", "r", 1*time.Second, "Delay between reconnect attempts")
	rootCmd.AddCommand(httpProxyCmd)
}
