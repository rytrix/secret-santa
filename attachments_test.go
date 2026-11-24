package main

import "testing"

func TestPictureExt(t *testing.T) {
	cases := []struct{ in, want string }{
		{"\xFF\xD8\xFF\xE0", ".jpg"},
		{"\x89PNG\r\n\x1a\n", ".png"},
		{"GIF89a", ".gif"},
		{"RIFF\x00\x00\x00\x00WEBPVP8 ", ".webp"},
		{"", ""},
		{"hello", ""},
		{"%PDF-1.7", ""},
		{"\x00\x00\x00\x18ftypheic", ""},
	}
	for _, c := range cases {
		if got := pictureExt([]byte(c.in)); got != c.want {
			t.Errorf("pictureExt(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
