package postgres_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"jacko/server/internal/attachments"
	attachmentpg "jacko/server/internal/attachments/postgres"
	attachments3 "jacko/server/internal/attachments/s3"
	"jacko/server/internal/communities"
	"jacko/server/internal/httpapi"
	"jacko/server/internal/messages"
)

func TestAttachmentsAPI(t *testing.T) {
	endpoint := os.Getenv("JACKO_TEST_S3_URL")
	if endpoint == "" {
		t.Skip("set JACKO_TEST_S3_URL and credentials to run S3 integration tests")
	}
	a := newResourceAPI(t)
	objects, err := attachments3.New(endpoint, os.Getenv("JACKO_TEST_S3_REGION"), os.Getenv("JACKO_TEST_S3_BUCKET"),
		os.Getenv("JACKO_TEST_S3_ACCESS_KEY"), os.Getenv("JACKO_TEST_S3_SECRET_KEY"))
	if err != nil {
		t.Fatal(err)
	}
	httpapi.RegisterAttachments(a.mux, attachments.NewService(attachmentpg.NewStore(a.pool), objects), a.auth, a.metrics)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		rows, err := a.pool.Query(ctx, "SELECT id, coalesce(object_key, '') FROM attachments")
		if err != nil {
			t.Error(err)
			return
		}
		defer rows.Close()
		for rows.Next() {
			var id, key string
			if err := rows.Scan(&id, &key); err != nil {
				t.Error(err)
				return
			}
			objects.Remove(ctx, "uploads/"+id)
			if key != "" {
				objects.Remove(ctx, key)
			}
		}
		if err := rows.Err(); err != nil {
			t.Error(err)
		}
	})
	owner, peer, outsider := a.owner.AccessToken, a.peer.AccessToken, a.outsider.AccessToken
	guild := a.guild(t)
	invite := decodeResource[communities.Invite](t, a.call(t, "POST", fmt.Sprintf("/api/v1/guilds/%d/invites", guild.Guild.ID), owner, "", 201))
	a.call(t, "POST", "/api/v1/invites/redeem", peer, fmt.Sprintf(`{"token":%q}`, invite.Token), 200)
	channel := fmt.Sprintf("/api/v1/channels/%d", guild.Channel.ID)
	begin := func(data []byte, media string) attachments.UploadForm {
		t.Helper()
		body := fmt.Sprintf(`{"media_type":%q,"size_bytes":%d}`, media, len(data))
		return decodeResource[attachments.UploadForm](t, a.call(t, "POST", channel+"/attachments", owner, body, 201))
	}
	a.call(t, "POST", channel+"/attachments", outsider, `{"media_type":"image/png","size_bytes":10}`, 404)
	a.call(t, "POST", channel+"/attachments", owner, `{"media_type":"image/svg+xml","size_bytes":10}`, 400)
	a.call(t, "POST", channel+"/attachments", owner, `{"media_type":"image/png","size_bytes":8388609}`, 400)

	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewNRGBA(image.Rect(0, 0, 2, 3))); err != nil {
		t.Fatal(err)
	}
	data := encoded.Bytes()
	form := begin(data, "image/png")
	assertUploadSize(t, form, int64(len(data)))
	path := "/api/v1/attachments/" + form.ID
	a.call(t, "POST", path+"/complete", peer, "", 404)
	a.call(t, "POST", path+"/complete", owner, "", 422)
	a.call(t, "GET", path, owner, "", 404)
	uploadObject(t, a.ctx, form, data)
	// Racing finalizations must converge on one immutable object.
	var results [2]*httptest.ResponseRecorder
	var group sync.WaitGroup
	for i := range results {
		group.Go(func() {
			request := httptest.NewRequest("POST", path+"/complete", nil).WithContext(a.ctx)
			request.Header.Set("Authorization", "Bearer "+owner)
			results[i] = httptest.NewRecorder()
			a.mux.ServeHTTP(results[i], request)
		})
	}
	group.Wait()
	for _, result := range results {
		if result.Code != 200 {
			t.Fatalf("complete returned %d: %s", result.Code, result.Body)
		}
		validated := decodeResource[attachments.Image](t, result.Body.Bytes())
		if validated.Width != 2 || validated.Height != 3 || validated.SizeBytes != int64(len(data)) {
			t.Fatalf("invalid metadata: %+v", validated)
		}
	}
	if !bytes.Equal(results[0].Body.Bytes(), results[1].Body.Bytes()) {
		t.Fatal("racing completion disagreed")
	}
	// A still-valid staging form cannot change already verified bytes.
	uploadObject(t, a.ctx, form, bytes.Repeat([]byte("x"), len(data)))
	a.call(t, "POST", path+"/complete", owner, "", 200)
	a.call(t, "GET", path, owner, "", 404) // not yet bound to a message
	requestBody := fmt.Sprintf(`{"nonce":"image","attachment_id":%q}`, form.ID)
	a.call(t, "POST", channel+"/messages", peer, requestBody, 404)
	other := a.guild(t)
	a.call(t, "POST", fmt.Sprintf("/api/v1/channels/%d/messages", other.Channel.ID), owner, requestBody, 404)
	message := decodeResource[messages.Message](t, a.call(t, "POST", channel+"/messages", owner, requestBody, 200))
	a.call(t, "POST", channel+"/messages", owner, requestBody, 200)
	a.call(t, "POST", channel+"/messages", owner, fmt.Sprintf(`{"nonce":"reuse","attachment_id":%q}`, form.ID), 404)
	a.call(t, "GET", path, outsider, "", 404)
	download := decodeResource[attachments.Download](t, a.call(t, "GET", path, peer, "", 200))
	request, err := http.NewRequestWithContext(a.ctx, "GET", download.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	downloaded, err := io.ReadAll(io.LimitReader(response.Body, attachments.MaxBytes+1))
	response.Body.Close()
	if err != nil || response.StatusCode != 200 || !bytes.Equal(downloaded, data) {
		t.Fatal("verified object was changed or is unavailable")
	}
	a.call(t, "DELETE", fmt.Sprintf("%s/messages/%d", channel, message.ID), owner, "", 204)
	a.call(t, "GET", path, peer, "", 404)
	a.call(t, "POST", channel+"/messages", owner, requestBody, 404)

	t.Run("InvalidImage", func(t *testing.T) {
		invalid := []byte("this is not a PNG")
		form := begin(invalid, "image/png")
		uploadObject(t, a.ctx, form, invalid)
		a.call(t, "POST", "/api/v1/attachments/"+form.ID+"/complete", owner, "", 422)
	})
	t.Run("OversizedDimensions", func(t *testing.T) {
		var data bytes.Buffer
		if err := png.Encode(&data, image.NewNRGBA(image.Rect(0, 0, 4097, 1))); err != nil {
			t.Fatal(err)
		}
		form := begin(data.Bytes(), "image/png")
		uploadObject(t, a.ctx, form, data.Bytes())
		a.call(t, "POST", "/api/v1/attachments/"+form.ID+"/complete", owner, "", 422)
	})
	t.Run("JPEG", func(t *testing.T) {
		var data bytes.Buffer
		if err := jpeg.Encode(&data, image.NewRGBA(image.Rect(0, 0, 2, 2)), nil); err != nil {
			t.Fatal(err)
		}
		form := begin(data.Bytes(), "image/jpeg")
		uploadObject(t, a.ctx, form, data.Bytes())
		a.call(t, "POST", "/api/v1/attachments/"+form.ID+"/complete", owner, "", 200)
		wrongType := begin(data.Bytes(), "image/png")
		uploadObject(t, a.ctx, wrongType, data.Bytes())
		a.call(t, "POST", "/api/v1/attachments/"+wrongType.ID+"/complete", owner, "", 422)
	})
	t.Run("ExpiredUpload", func(t *testing.T) {
		form := begin(data, "image/png")
		if _, err := a.pool.Exec(a.ctx, "UPDATE attachments SET upload_expires_at = now() - interval '1 second' WHERE id = $1", form.ID); err != nil {
			t.Fatal(err)
		}
		a.call(t, "POST", "/api/v1/attachments/"+form.ID+"/complete", owner, "", 404)
	})
}

func assertUploadSize(t *testing.T, form attachments.UploadForm, size int64) {
	t.Helper()
	policy, err := base64.StdEncoding.DecodeString(form.Fields["policy"])
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Conditions []json.RawMessage `json:"conditions"`
	}
	if err := json.Unmarshal(policy, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, condition := range decoded.Conditions {
		var values []any
		if json.Unmarshal(condition, &values) == nil && len(values) == 3 && values[0] == "content-length-range" {
			if values[1] != float64(size) || values[2] != float64(size) {
				t.Fatalf("upload policy exceeds reservation: %s", condition)
			}
			return
		}
	}
	t.Fatal("missing signed size restriction")
}

func uploadObject(t *testing.T, ctx context.Context, form attachments.UploadForm, data []byte) {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for key, value := range form.Fields {
		if err := writer.WriteField(key, value); err != nil {
			t.Fatal(err)
		}
	}
	file, err := writer.CreateFormFile("file", "image")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequestWithContext(ctx, "POST", form.URL, &body)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", writer.FormDataContentType())
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		t.Fatalf("S3 upload returned %d: %s", response.StatusCode, message)
	}
}
