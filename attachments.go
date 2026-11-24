package main

// Gift attachments: downloading uploads, storing them until /santa end, and
// sending them in DMs.
//
// Why the bot keeps its own copy: links to files uploaded to Discord expire
// after about a day, and gifts can wait weeks for /santa end.
//
// Layout on disk (mirrored inside each backup folder):
//
//	santa-attachments/<guildID>-<round created, unix ns>/<santaID>-<random>-<n>.<ext>

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/bwmarrin/discordgo"
)

const (
	maxAttachments     = 3  // per gift
	maxFilesPerMessage = 10 // Discord's attachment limit per message
	// Discord's upload limit for bots has changed over the years (8, 10, 25
	// MiB at various points; it now depends on the server). 8 MiB per attachment
	// and per message stays under every limit it has had.
	maxAttachmentBytes = 8 << 20
	maxMessageBytes = 8 << 20
)

var httpClient = &http.Client{Timeout: 60 * time.Second}

// fetchAttachment downloads one uploaded attachment and checks it's an image
// type we can send, returning the bytes and the file extension to store it under.
func fetchAttachment(a *discordgo.MessageAttachment) ([]byte, string, error) {
	tooBig := userErr(fmt.Sprintf("**%s** is too big; the limit is %d MB per attachment.", a.Filename, maxAttachmentBytes>>20))
	if a.Size > maxAttachmentBytes {
		return nil, "", tooBig
	}
	resp, err := httpClient.Get(a.URL)
	if err != nil {
		return nil, "", fmt.Errorf("downloading %s: %w", a.Filename, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("downloading %s: %s", a.Filename, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxAttachmentBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("downloading %s: %w", a.Filename, err)
	}
	if len(b) > maxAttachmentBytes {
		return nil, "", tooBig
	}
	ext := pictureExt(b)
	if ext == "" {
		return nil, "", userErr(fmt.Sprintf("**%s** can't be sent: attachments must be JPEG, PNG, GIF or WebP images.", a.Filename))
	}
	return b, ext, nil
}

// pictureExt identifies the image format from the file's first bytes rather
// than trusting the uploaded filename or content type. Returns "" if it's not
// a JPEG, PNG, GIF or WebP.
func pictureExt(b []byte) string {
	switch {
	case bytes.HasPrefix(b, []byte{0xFF, 0xD8, 0xFF}):
		return ".jpg"
	case bytes.HasPrefix(b, []byte("\x89PNG\r\n\x1a\n")):
		return ".png"
	case bytes.HasPrefix(b, []byte("GIF87a")), bytes.HasPrefix(b, []byte("GIF89a")):
		return ".gif"
	case len(b) >= 12 && string(b[0:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		return ".webp"
	}
	return ""
}

func contentTypeOf(ext string) string {
	switch ext {
	case ".jpg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	}
	return "application/octet-stream"
}

// saveAttachments downloads and stores attachments in dir, returning the
// stored file names in upload order. On any failure nothing is left behind.
func saveAttachments(dir, santa string, atts []*discordgo.MessageAttachment) ([]string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	// A random tag per upload so a replacement gift never overwrites the
	// files of the one it replaces before the swap is saved.
	tag := make([]byte, 4)
	rand.Read(tag)
	var names []string
	for k, a := range atts {
		data, ext, err := fetchAttachment(a)
		if err == nil {
			name := fmt.Sprintf("%s-%s-%d%s", santa, hex.EncodeToString(tag), k+1, ext)
			err = os.WriteFile(filepath.Join(dir, name), data, 0o600)
			if err == nil {
				names = append(names, name)
				continue
			}
		}
		removeFiles(dir, names)
		return nil, err
	}
	return names, nil
}

func removeFiles(dir string, names []string) {
	for _, n := range names {
		if err := os.Remove(filepath.Join(dir, n)); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Printf("removing %s: %v", n, err)
		}
	}
}

// batchAttachments groups attachments into messages of at most maxFilesPerMessage files
// and maxMessageBytes total.
func batchAttachments(dir string, names []string) ([][]string, error) {
	var batches [][]string
	var cur []string
	var size int64
	for _, n := range names {
		st, err := os.Stat(filepath.Join(dir, n))
		if err != nil {
			return nil, err
		}
		if len(cur) > 0 && (len(cur) == maxFilesPerMessage || size+st.Size() > maxMessageBytes) {
			batches = append(batches, cur)
			cur, size = nil, 0
		}
		cur = append(cur, n)
		size += st.Size()
	}
	if len(cur) > 0 {
		batches = append(batches, cur)
	}
	return batches, nil
}

// sendGift DMs text plus attachments to userID. The text goes with the first
// batch of attachments; any further batches follow as extra messages. Attachments
// are renamed gift-1.jpg, gift-2.png… so the Santa's own filenames (which can
// contain names or dates) are never shown.
func sendGift(s *discordgo.Session, userID, text, dir string, attachments []string) error {
	batches, err := batchAttachments(dir, attachments)
	if err != nil {
		return err
	}
	if len(batches) == 0 {
		batches = [][]string{nil} // text only
	}
	ch, err := s.UserChannelCreate(userID)
	if err != nil {
		return err
	}
	num := 0
	for k, batch := range batches {
		msg := &discordgo.MessageSend{AllowedMentions: &discordgo.MessageAllowedMentions{}}
		if k == 0 {
			msg.Content = text
		}
		var open []*os.File
		for _, name := range batch {
			f, err := os.Open(filepath.Join(dir, name))
			if err != nil {
				closeAll(open)
				return err
			}
			open = append(open, f)
			num++
			ext := filepath.Ext(name)
			msg.Files = append(msg.Files, &discordgo.File{
				Name:        fmt.Sprintf("gift-%d%s", num, ext),
				ContentType: contentTypeOf(ext),
				Reader:      f,
			})
		}
		_, err := s.ChannelMessageSendComplex(ch.ID, msg)
		closeAll(open)
		if err != nil {
			return err
		}
	}
	return nil
}

func closeAll(files []*os.File) {
	for _, f := range files {
		f.Close()
	}
}
