package main

import "strings"

type deprecatedAppInfo struct {
	Message     string `json:"message"`
	Replacement string `json:"replacement,omitempty"`
}

var deprecatedApps = map[string]deprecatedAppInfo{
	"hosting": {
		Message:     "Hosting has been retired. Use the newer Fleet, Containers, SaaS, and server-native ingress pieces instead.",
		Replacement: "fleet, containers, saas",
	},
	"routes": {
		Message:     "Routes has been folded into apteva-server. New apps should use server-native ingress through PlatformAPI().ExposeIngress.",
		Replacement: "server-native ingress",
	},
	"certs": {
		Message:     "Certs has been folded into apteva-server. Server-native ingress now owns managed TLS for exposed hostnames.",
		Replacement: "server-native ingress TLS",
	},
}

func deprecatedApp(name string) (deprecatedAppInfo, bool) {
	info, ok := deprecatedApps[strings.ToLower(strings.TrimSpace(name))]
	return info, ok
}
