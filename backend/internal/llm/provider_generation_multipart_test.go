package llm

import (
	"bytes"
	"io"
	"mime"
	"mime/multipart"
	"testing"
)

func TestEncodeGenerationMultipart_KeepsDeclaredPNG(t *testing.T) {
	png := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0x00}
	body, contentType, err := encodeGenerationMultipart("gpt-image-2", &MultipartInput{
		Fields: map[string][]string{
			"model":  {"gpt-image-1"},
			"prompt": {"replace the background"},
		},
		Files: []MultipartFile{{
			Field:       "image",
			Filename:    "source.png",
			ContentType: "image/png",
			Content:     png,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	gotType, gotBody := generationMultipartPart(t, contentType, body, "image")
	if gotType != "image/png" {
		t.Fatalf("content type = %q, want image/png", gotType)
	}
	if !bytes.Equal(gotBody, png) {
		t.Fatalf("image bytes changed: %d vs %d", len(gotBody), len(png))
	}
	if model := generationMultipartField(t, contentType, body, "model"); model != "gpt-image-2" {
		t.Fatalf("model = %q, want gpt-image-2", model)
	}
}

func TestEncodeGenerationMultipart_SniffsOctetStreamPNG(t *testing.T) {
	png := append([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}, bytes.Repeat([]byte{0x01}, 600)...)
	body, contentType, err := encodeGenerationMultipart("gpt-image-2", &MultipartInput{
		Fields: map[string][]string{"prompt": {"edit"}},
		Files: []MultipartFile{{
			Field:       "image",
			Filename:    "upload.bin",
			ContentType: "application/octet-stream",
			Content:     png,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	gotType, gotBody := generationMultipartPart(t, contentType, body, "image")
	if gotType != "image/png" {
		t.Fatalf("content type = %q, want image/png", gotType)
	}
	if !bytes.Equal(gotBody, png) {
		t.Fatal("png bytes were not preserved")
	}
}

func TestEncodeGenerationMultipart_RealJPEGOverridesPNGLabel(t *testing.T) {
	jpeg := []byte{0xff, 0xd8, 0xff, 0x00, 0x11}
	body, contentType, err := encodeGenerationMultipart("gpt-image-2", &MultipartInput{
		Files: []MultipartFile{{
			Field:       "image",
			Filename:    "source.png",
			ContentType: "image/png",
			Content:     jpeg,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	gotType, _ := generationMultipartPart(t, contentType, body, "image")
	if gotType != "image/jpeg" {
		t.Fatalf("content type = %q, want image/jpeg", gotType)
	}
}

func TestEncodeGenerationMultipart_MultipleImagesUseArrayField(t *testing.T) {
	png := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0x00}
	mask := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0x01}
	body, contentType, err := encodeGenerationMultipart("gpt-image-2", &MultipartInput{
		Fields: map[string][]string{"prompt": {"combine the references"}},
		Files: []MultipartFile{
			{Field: "image", Filename: "a.png", ContentType: "image/png", Content: png},
			{Field: "image", Filename: "b.png", ContentType: "image/png", Content: png},
			{Field: "mask", Filename: "mask.png", ContentType: "image/png", Content: mask},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	names := generationMultipartFileNames(t, contentType, body)
	if len(names) != 3 || names[0] != "image[]" || names[1] != "image[]" || names[2] != "mask" {
		t.Fatalf("file fields = %q, want [image[] image[] mask]", names)
	}
	if _, ok := generationMultipartFile(t, contentType, body, "image"); ok {
		t.Fatal("repeated image field was still forwarded")
	}
}

func TestEncodeGenerationMultipart_KeepsDeclaredAudio(t *testing.T) {
	audio := []byte("not-a-recognized-audio-header")
	body, contentType, err := encodeGenerationMultipart("whisper-1", &MultipartInput{
		Files: []MultipartFile{{
			Field:       "file",
			Filename:    "meeting.mp3",
			ContentType: "audio/mpeg",
			Content:     audio,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	gotType, gotBody := generationMultipartPart(t, contentType, body, "file")
	if gotType != "audio/mpeg" {
		t.Fatalf("content type = %q, want audio/mpeg", gotType)
	}
	if !bytes.Equal(gotBody, audio) {
		t.Fatal("audio bytes were not preserved")
	}
}

func generationMultipartFileNames(t *testing.T, contentType string, body []byte) []string {
	t.Helper()
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		t.Fatal(err)
	}
	reader := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	var names []string
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if part.FileName() != "" {
			names = append(names, part.FormName())
		}
		_ = part.Close()
	}
	return names
}

func generationMultipartFile(t *testing.T, contentType string, body []byte, field string) ([]byte, bool) {
	t.Helper()
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		t.Fatal(err)
	}
	reader := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			return nil, false
		}
		if err != nil {
			t.Fatal(err)
		}
		if part.FormName() == field && part.FileName() != "" {
			data, err := io.ReadAll(part)
			_ = part.Close()
			if err != nil {
				t.Fatal(err)
			}
			return data, true
		}
		_ = part.Close()
	}
}

func generationMultipartPart(t *testing.T, contentType string, body []byte, field string) (string, []byte) {
	t.Helper()
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		t.Fatal(err)
	}
	reader := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if part.FormName() == field && part.FileName() != "" {
			data, err := io.ReadAll(part)
			_ = part.Close()
			if err != nil {
				t.Fatal(err)
			}
			return part.Header.Get("Content-Type"), data
		}
		_ = part.Close()
	}
	t.Fatalf("file field %q not found", field)
	return "", nil
}

func generationMultipartField(t *testing.T, contentType string, body []byte, field string) string {
	t.Helper()
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		t.Fatal(err)
	}
	reader := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if part.FormName() == field && part.FileName() == "" {
			data, err := io.ReadAll(part)
			_ = part.Close()
			if err != nil {
				t.Fatal(err)
			}
			return string(data)
		}
		_ = part.Close()
	}
	t.Fatalf("field %q not found", field)
	return ""
}
