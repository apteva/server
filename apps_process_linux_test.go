//go:build linux

package main

import (
	"strings"
	"testing"
)

func TestLinuxProcessBirthParsesKernelStat(t *testing.T) {
	fields := append([]string{"S"}, strings.Fields(strings.Repeat("0 ", 18))...)
	fields = append(fields, "123456")
	raw := []byte("123 (a tricky ) name (with spaces)) " + strings.Join(fields, " "))
	if birth, err := linuxProcessBirth(raw, 123); err != nil || birth != "123456" {
		t.Fatalf("birth=%q err=%v", birth, err)
	}
	for _, value := range []string{"", "321 (wrong pid) " + strings.Join(fields, " "), "123 (short) S 0", "123 (zombie) Z " + strings.Join(fields[1:], " ")} {
		if _, err := linuxProcessBirth([]byte(value), 123); err == nil {
			t.Fatalf("accepted invalid stat %q", value)
		}
	}
}
