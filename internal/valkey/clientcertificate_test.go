package valkey

import "testing"

func TestClientCertificateRefused(t *testing.T) {
	cases := []struct {
		name string
		err  string
		want bool
	}{
		{"tls13 certificate required", "remote error: tls: certificate required", true},
		{"tls12 handshake failure", "remote error: tls: handshake failure", true},
		{"wrongpass is not a cert refusal", "WRONGPASS invalid username-password pair", false},
		{"dial refused is not a cert refusal", "dial tcp 127.0.0.1:6379: connect: connection refused", false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		if got := ClientCertificateRefused(errorString(tc.err)); got != tc.want {
			t.Errorf("%s: ClientCertificateRefused(%q) = %v, want %v", tc.name, tc.err, got, tc.want)
		}
	}
	if ClientCertificateRefused(nil) {
		t.Error("nil error must not be a cert refusal")
	}
}

type errorString string

func (e errorString) Error() string { return string(e) }
