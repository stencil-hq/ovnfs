package main

import "testing"

func TestMountSettingsNFSIOSize(t *testing.T) {
	for _, size := range []int{0, 4 << 10, 8 << 10, 64 << 10, 128 << 10, 1 << 20} {
		s := mountSettings{NFSIOSize: size}
		if err := s.validate(); err != nil {
			t.Errorf("size %d: %v", size, err)
		}
		want := size
		if want == 0 {
			want = defaultNFSIOSize
		}
		if got := s.nfsIOSize(); got != want {
			t.Errorf("size %d resolves to %d, want %d", size, got, want)
		}
	}
	for _, size := range []int{-1, 1, 2048, 4095, 4097, 12 << 10, (1 << 20) + 1, 2 << 20} {
		if err := (mountSettings{NFSIOSize: size}).validate(); err == nil {
			t.Errorf("accepted invalid size %d", size)
		}
	}
}
