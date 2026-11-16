package wac

import (
	"bytes"
	"testing"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
)

// A new envelope must remain readable and downloadable after protobuf storage,
// without opening a client, accessing a profile, or downloading any media.
func TestVideoEnvelopes(t *testing.T) {
	for _, kind := range []string{"video", "instant", "animation", "video_note"} {
		t.Run(kind, func(t *testing.T) {
			v := &waE2E.VideoMessage{
				Caption: proto.String("synthetic caption"), Mimetype: proto.String("video/mp4"),
				FileLength: proto.Uint64(1234), Seconds: proto.Uint32(7),
				JPEGThumbnail: []byte("synthetic thumbnail"),
				ContextInfo:   &waE2E.ContextInfo{StanzaID: proto.String("synthetic reply")},
			}
			m := &waE2E.Message{}
			wantKind := "video"
			switch kind {
			case "video":
				m.VideoMessage = v
			case "instant":
				m.InstantVideoMessage = v
			case "animation":
				m.InstantVideoMessage = v
				v.GifPlayback = proto.Bool(true)
				wantKind = "animation"
			case "video_note":
				m.PtvMessage = v
				wantKind = "video_note"
			}
			content := Parse(m)
			if content.Empty() || content.Kind != wantKind || content.Text != v.GetCaption() || content.Context.GetStanzaID() != "synthetic reply" {
				t.Fatalf("content: %+v", content)
			}
			raw, err := proto.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			media, download, err := MediaInfo(&Message{Raw: raw, Kind: content.Kind, Text: content.Text})
			if err != nil {
				t.Fatal(err)
			}
			dv, ok := download.(*waE2E.VideoMessage)
			if !ok || !proto.Equal(v, dv) {
				t.Fatal("download target differs from stored video")
			}
			if media.Kind != wantKind || media.Caption != v.GetCaption() || media.MIME != "video/mp4" || media.Size != 1234 || media.Seconds != 7 || !bytes.Equal(media.Thumb, v.JPEGThumbnail) {
				t.Fatalf("media: %+v", media)
			}
		})
	}
}
