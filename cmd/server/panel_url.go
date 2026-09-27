package main

import (
	"net"
	"strings"
)

// panelURL builds the loopback management URL for the tray menu. Wildcard
// listeners use 127.0.0.1 because the browser runs on the same machine.
func panelURL(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		if value, ok := strings.CutPrefix(listen, ":"); ok && value != "" {
			host, port = "", value
		} else {
			host, port = "127.0.0.1", "7863"
		}
	}
	switch strings.Trim(host, "[]") {
	case "", "0.0.0.0", "::", "*":
		host = "127.0.0.1"
	}
	if port == "" {
		port = "7863"
	}
	return "http://" + net.JoinHostPort(host, port) + "/panel/"
}
