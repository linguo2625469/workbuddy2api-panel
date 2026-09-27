package main

import "testing"

func TestPanelURL(t *testing.T) {
	tests := []struct {
		listen string
		want   string
	}{
		{listen: ":7863", want: "http://127.0.0.1:7863/panel/"},
		{listen: "0.0.0.0:7863", want: "http://127.0.0.1:7863/panel/"},
		{listen: "[::]:7863", want: "http://127.0.0.1:7863/panel/"},
		{listen: "127.0.0.1:7863", want: "http://127.0.0.1:7863/panel/"},
		{listen: "localhost:7863", want: "http://localhost:7863/panel/"},
		{listen: "[::1]:7863", want: "http://[::1]:7863/panel/"},
		{listen: "invalid", want: "http://127.0.0.1:7863/panel/"},
	}
	for _, tt := range tests {
		t.Run(tt.listen, func(t *testing.T) {
			if got := panelURL(tt.listen); got != tt.want {
				t.Fatalf("panelURL(%q) = %q, want %q", tt.listen, got, tt.want)
			}
		})
	}
}
