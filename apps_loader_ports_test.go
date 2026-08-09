package main

import "testing"

func TestSelectPrimaryHostPort(t *testing.T) {
	ports := []orchestratorPort{
		{HostPort: 1883, ContainerPort: 1883},
		{HostPort: 49152, ContainerPort: 8080},
	}
	if got := selectPrimaryHostPort(ports, 8080); got != 49152 {
		t.Fatalf("got %d, want HTTP host port 49152", got)
	}
}

func TestSelectPrimaryHostPortLegacySinglePort(t *testing.T) {
	if got := selectPrimaryHostPort([]orchestratorPort{{HostPort: 49152}}, 8080); got != 49152 {
		t.Fatalf("got %d, want legacy host port 49152", got)
	}
}

func TestSelectPrimaryHostPortDoesNotGuessWithMultiplePorts(t *testing.T) {
	ports := []orchestratorPort{{HostPort: 1883}, {HostPort: 49152}}
	if got := selectPrimaryHostPort(ports, 8080); got != 0 {
		t.Fatalf("got %d, want no ambiguous match", got)
	}
}
