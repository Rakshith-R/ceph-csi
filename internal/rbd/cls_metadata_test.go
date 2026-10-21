/*
Copyright 2026 The Ceph-CSI Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package rbd

import (
	"bytes"
	"testing"
)

func TestClsEncodeString(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want []byte
	}{
		{
			name: "empty",
			in:   "",
			want: []byte{0, 0, 0, 0},
		},
		{
			name: "short",
			in:   "abc",
			want: []byte{3, 0, 0, 0, 'a', 'b', 'c'},
		},
		{
			name: "256 bytes",
			in:   string(bytes.Repeat([]byte{'x'}, 256)),
			want: append([]byte{0, 1, 0, 0}, bytes.Repeat([]byte{'x'}, 256)...),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := clsEncodeString(nil, tt.in)
			if !bytes.Equal(got, tt.want) {
				t.Errorf("clsEncodeString(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}

	// appends to the existing buffer
	got := clsEncodeString([]byte{0xff}, "a")
	want := []byte{0xff, 1, 0, 0, 0, 'a'}
	if !bytes.Equal(got, want) {
		t.Errorf("clsEncodeString() = %v, want %v", got, want)
	}
}

func TestClsEncodeMetadataSet(t *testing.T) {
	t.Parallel()

	got := clsEncodeMetadataSet("key", "value")
	want := []byte{
		1, 0, 0, 0, // map count
		3, 0, 0, 0, 'k', 'e', 'y',
		5, 0, 0, 0, 'v', 'a', 'l', 'u', 'e',
	}
	if !bytes.Equal(got, want) {
		t.Errorf("clsEncodeMetadataSet() = %v, want %v", got, want)
	}

	got = clsEncodeMetadataSet("k", "")
	want = []byte{
		1, 0, 0, 0,
		1, 0, 0, 0, 'k',
		0, 0, 0, 0,
	}
	if !bytes.Equal(got, want) {
		t.Errorf("clsEncodeMetadataSet() = %v, want %v", got, want)
	}
}

func TestIsLibrbdManagedMetadataKey(t *testing.T) {
	t.Parallel()

	tests := map[string]bool{
		metadataConfPrefix + iopsLimit: true,
		"conf_rbd_cache":               true,
		baseIops:                       false,
		clusterNameKey:                 false,
		mounterKey:                     false,
		"rbd.csi.ceph.com/encrypted":   false,
		".rbd.csi.ceph.com/conf_":      false,
		"":                             false,
	}
	for key, want := range tests {
		if got := isLibrbdManagedMetadataKey(key); got != want {
			t.Errorf("isLibrbdManagedMetadataKey(%q) = %v, want %v", key, got, want)
		}
	}
}
