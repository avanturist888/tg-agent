package tgc

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gotd/td/telegram/uploader"
	"github.com/gotd/td/tg"
)

// audioAttribute reads duration and optional tags from a local snapshot.
// ffprobe is shared with the installation's FFmpeg tools; no shell is used.
func audioAttribute(ctx context.Context, path string) (*tg.DocumentAttributeAudio, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-protocol_whitelist", "file,pipe",
		"-select_streams", "a:0", "-show_entries", "stream=codec_type:format=duration:format_tags=title,artist", "-of", "json", path)
	hideAudioProbe(cmd)
	raw, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("не удалось прочитать аудио через ffprobe (FFmpeg): %w", err)
	}
	return parseAudioAttribute(raw, filepath.Base(path))
}

func parseAudioAttribute(raw []byte, name string) (*tg.DocumentAttributeAudio, error) {
	var info struct {
		Streams []struct {
			CodecType string `json:"codec_type"`
		} `json:"streams"`
		Format struct {
			Duration string            `json:"duration"`
			Tags     map[string]string `json:"tags"`
		} `json:"format"`
	}
	if err := json.Unmarshal(raw, &info); err != nil {
		return nil, fmt.Errorf("не удалось разобрать метаданные аудио: %w", err)
	}
	seconds, err := strconv.ParseFloat(info.Format.Duration, 64)
	if err != nil || math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds <= 0 || math.Ceil(seconds) > math.MaxInt32 || len(info.Streams) == 0 || info.Streams[0].CodecType != "audio" {
		return nil, fmt.Errorf("в файле нет аудиодорожки с известной длительностью")
	}
	title := strings.TrimSuffix(name, filepath.Ext(name))
	performer := ""
	for key, value := range info.Format.Tags {
		if value = strings.TrimSpace(value); value == "" {
			continue
		}
		switch strings.ToLower(key) {
		case "title":
			title = value
		case "artist":
			performer = value
		}
	}
	a := &tg.DocumentAttributeAudio{Duration: int(math.Ceil(seconds))}
	a.SetTitle(title)
	if performer != "" {
		a.SetPerformer(performer)
	}
	return a, nil
}

func (c *Conn) sendAudio(ctx context.Context, t Target, path string, reply *int64) ([]int, error) {
	a, err := audioAttribute(ctx, path)
	if err != nil {
		return nil, &PreSend{err}
	}
	file, err := uploader.NewUploader(c.API).FromPath(ctx, path)
	if err != nil {
		return nil, &PreSend{err}
	}
	mime := "audio/mp4"
	if strings.EqualFold(filepath.Ext(path), ".mp3") {
		mime = "audio/mpeg"
	}
	req := &tg.MessagesSendMediaRequest{
		Peer: t.Input,
		Media: &tg.InputMediaUploadedDocument{
			File: file, MimeType: mime,
			Attributes: []tg.DocumentAttributeClass{a, &tg.DocumentAttributeFilename{FileName: filepath.Base(path)}},
		},
		RandomID: randomID(),
	}
	if t.ScheduleIn > 0 {
		req.SetScheduleDate(int(time.Now().Add(t.ScheduleIn).Unix()))
	}
	if r := replyTo(reply, t.Topic); r != nil {
		req.ReplyTo = r
	}
	res, err := c.API.MessagesSendMedia(ctx, req)
	if err != nil {
		return nil, err
	}
	ids, _ := sent(res, []int64{req.RandomID})
	return ids, nil
}
