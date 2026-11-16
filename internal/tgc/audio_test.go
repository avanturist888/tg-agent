package tgc

import "testing"

func TestAudioPlayerMetadata(t *testing.T) {
	for _, name := range []string{"01 - chapter.mp3", "01 - chapter.m4a", "01 - chapter.m4b"} {
		a, err := parseAudioAttribute([]byte(`{"streams":[{"codec_type":"audio"}],"format":{"duration":"61.25"}}`), name)
		if err != nil {
			t.Fatal(err)
		}
		title, ok := a.GetTitle()
		if !ok || title != "01 - chapter" || a.Duration != 62 || a.Voice {
			t.Fatalf("audio player attributes: %+v", a)
		}
	}
	a, err := parseAudioAttribute([]byte(`{"streams":[{"codec_type":"audio"}],"format":{"duration":"2","tags":{"TITLE":" Tagged chapter ","artist":" Reader "}}}`), "file.mp3")
	if err != nil {
		t.Fatal(err)
	}
	title, _ := a.GetTitle()
	performer, ok := a.GetPerformer()
	if title != "Tagged chapter" || !ok || performer != "Reader" || a.Duration != 2 || a.Voice {
		t.Fatalf("audio tags: %+v", a)
	}
}

func TestInvalidAudioMetadata(t *testing.T) {
	for _, raw := range []string{
		`not json`,
		`{"streams":[],"format":{"duration":"2"}}`,
		`{"streams":[{"codec_type":"video"}],"format":{"duration":"2"}}`,
		`{"streams":[{"codec_type":"audio"}],"format":{"duration":"NaN"}}`,
		`{"streams":[{"codec_type":"audio"}],"format":{"duration":"+Inf"}}`,
		`{"streams":[{"codec_type":"audio"}],"format":{"duration":"0"}}`,
		`{"streams":[{"codec_type":"audio"}],"format":{"duration":"-1"}}`,
		`{"streams":[{"codec_type":"audio"}],"format":{"duration":"999999999999999"}}`,
	} {
		if _, err := parseAudioAttribute([]byte(raw), "file.mp3"); err == nil {
			t.Fatal("invalid audio metadata accepted")
		}
	}
}
