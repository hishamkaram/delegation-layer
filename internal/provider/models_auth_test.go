package provider

import "testing"

func TestModelAuthFailureRecognizesExplicitAuthorizationStatuses(t *testing.T) {
	cases := []struct {
		name string
		data string
		want bool
	}{
		{name: "numeric error code", data: `{"error":{"code":403}}`, want: true},
		{name: "numeric status", data: `{"status":401}`, want: true},
		{name: "string status in json lines", data: "warning\n{\"status_code\":\"403\"}\n", want: true},
		{name: "forbidden text", data: "403 Forbidden", want: true},
		{name: "server failure", data: `{"code":500,"message":"permission denied"}`, want: false},
		{name: "generic permission text", data: "permission denied", want: false},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := ModelAuthFailure([]byte(test.data), nil); got != test.want {
				t.Fatalf("ModelAuthFailure(%q)=%v, want %v", test.data, got, test.want)
			}
		})
	}
}
