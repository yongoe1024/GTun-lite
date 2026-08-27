package common

import (
	"bytes"
	"testing"
)

func TestProbeRequestRoundTripAndBoundaries(t *testing.T) {
	request := ProbeRequest{Nonce: "0123456789abcdef", ProbeID: 5}
	encoded, err := EncodeProbeRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(encoded, []byte("PROBE 0123456789abcdef 5")) {
		t.Fatalf("encoded request = %q", encoded)
	}
	decoded, err := ParseProbeRequest(encoded)
	if err != nil || decoded != request {
		t.Fatalf("decoded request = %#v, %v", decoded, err)
	}
	invalid := []string{
		"",
		"PROBE 0123456789abcde 1",
		"PROBE 0123456789ABCDE 1",
		"PROBE 0123456789abcdef 0",
		"PROBE 0123456789abcdef 6",
		"PROBE 0123456789abcdef 01",
		"PROBE 0123456789abcdef 1 extra",
		" PROBE 0123456789abcdef 1",
		"PROBE 0123456789abcdef 1\n",
	}
	for _, value := range invalid {
		if _, err := ParseProbeRequest([]byte(value)); err == nil {
			t.Errorf("invalid request accepted: %q", value)
		}
	}
	tooLong := bytes.Repeat([]byte("x"), MaxProbeDatagram+1)
	if _, err := ParseProbeRequest(tooLong); err == nil {
		t.Fatal("oversized request accepted")
	}
}

func TestProbeResponseRoundTripAndValidation(t *testing.T) {
	response := ProbeResponse{Nonce: "fedcba9876543210", ProbeID: 1, PublicIP: "198.51.100.10", MappedPort: 65535}
	encoded, err := EncodeProbeResponse(response)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := ParseProbeResponse(encoded)
	if err != nil || decoded != response {
		t.Fatalf("decoded response = %#v, %v", decoded, err)
	}
	for _, value := range []string{
		"PORT fedcba9876543210 1 198.51.100.10 0",
		"PORT fedcba9876543210 1 198.51.100.10 65536",
		"PORT fedcba9876543210 1 198.51.100.999 1",
		"PORT fedcba9876543210 01 198.51.100.10 1",
		"PORT fedcba9876543210 1 198.51.100.10 1\n",
	} {
		if _, err := ParseProbeResponse([]byte(value)); err == nil {
			t.Errorf("invalid response accepted: %q", value)
		}
	}
}
