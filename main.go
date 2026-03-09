package main

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/meet/v2"
	"google.golang.org/api/option"
)

//go:embed templates/*.html
var templateFS embed.FS

var tmpl = template.Must(template.ParseFS(templateFS, "templates/*.html"))

// Session store (in-memory, PoC only)
var (
	sessions   = map[string]*oauth2.Token{}
	sessionsMu sync.RWMutex
)

func oauthConfig() *oauth2.Config {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	return &oauth2.Config{
		ClientID:     os.Getenv("GOOGLE_CLIENT_ID"),
		ClientSecret: os.Getenv("GOOGLE_CLIENT_SECRET"),
		Endpoint:     google.Endpoint,
		RedirectURL:  fmt.Sprintf("http://localhost:%s/callback", port),
		Scopes: []string{
			"https://www.googleapis.com/auth/meetings.space.readonly",
			"https://www.googleapis.com/auth/calendar.readonly",
		},
	}
}

// pageData holds template rendering data.
type pageData struct {
	Authenticated bool
	MeetingCode   string
	Transcript    string
	Error         string
	Metadata      *transcriptMeta
}

type transcriptMeta struct {
	EntryCount int
	Language   string
	StartTime  string
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	if os.Getenv("GOOGLE_CLIENT_ID") == "" || os.Getenv("GOOGLE_CLIENT_SECRET") == "" {
		log.Fatal("GOOGLE_CLIENT_ID and GOOGLE_CLIENT_SECRET must be set")
	}

	http.HandleFunc("/", handleIndex)
	http.HandleFunc("/auth", handleAuth)
	http.HandleFunc("/callback", handleCallback)
	http.HandleFunc("/transcript", handleTranscript)
	http.HandleFunc("/logout", handleLogout)

	log.Printf("Starting server on http://localhost:%s", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	data := pageData{
		Authenticated: getToken(r) != nil,
	}
	renderTemplate(w, data)
}

func handleAuth(w http.ResponseWriter, r *http.Request) {
	cfg := oauthConfig()
	// Use "consent" prompt to always get refresh_token
	url := cfg.AuthCodeURL("state", oauth2.AccessTypeOffline, oauth2.SetAuthURLParam("prompt", "consent"))
	http.Redirect(w, r, url, http.StatusTemporaryRedirect)
}

func handleCallback(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("code")
	if code == "" {
		renderError(w, "認証コードが取得できませんでした")
		return
	}

	cfg := oauthConfig()
	token, err := cfg.Exchange(context.Background(), code)
	if err != nil {
		renderError(w, fmt.Sprintf("トークン交換に失敗しました: %v", err))
		return
	}

	// Create session
	sessionID := generateSessionID()
	sessionsMu.Lock()
	sessions[sessionID] = token
	sessionsMu.Unlock()

	http.SetCookie(w, &http.Cookie{
		Name:     "session_id",
		Value:    sessionID,
		Path:     "/",
		HttpOnly: true,
	})

	http.Redirect(w, r, "/", http.StatusTemporaryRedirect)
}

func handleTranscript(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	token := getToken(r)
	if token == nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	meetingCode := strings.TrimSpace(r.FormValue("meeting_code"))
	if meetingCode == "" {
		renderPage(w, pageData{
			Authenticated: true,
			Error:         "Meeting Codeを入力してください",
		})
		return
	}

	// Fetch transcript using Meet API
	transcript, meta, err := fetchTranscript(r.Context(), token, meetingCode)
	if err != nil {
		renderPage(w, pageData{
			Authenticated: true,
			MeetingCode:   meetingCode,
			Error:         fmt.Sprintf("文字起こし取得に失敗しました: %v", err),
		})
		return
	}

	renderPage(w, pageData{
		Authenticated: true,
		MeetingCode:   meetingCode,
		Transcript:    transcript,
		Metadata:      meta,
	})
}

func handleLogout(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie("session_id")
	if err == nil {
		sessionsMu.Lock()
		delete(sessions, cookie.Value)
		sessionsMu.Unlock()
	}

	http.SetCookie(w, &http.Cookie{
		Name:   "session_id",
		Value:  "",
		Path:   "/",
		MaxAge: -1,
	})

	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// fetchTranscript retrieves transcript from Google Meet API.
func fetchTranscript(ctx context.Context, token *oauth2.Token, meetingCode string) (string, *transcriptMeta, error) {
	cfg := oauthConfig()
	httpClient := cfg.Client(ctx, token)

	meetService, err := meet.NewService(ctx, option.WithHTTPClient(httpClient))
	if err != nil {
		return "", nil, fmt.Errorf("Meet service creation failed: %w", err)
	}

	// Step 1: Find conference records by meeting code
	filter := fmt.Sprintf("meeting_code=%q", meetingCode)
	listResp, err := meetService.ConferenceRecords.List().Filter(filter).Do()
	if err != nil {
		return "", nil, fmt.Errorf("conference record search failed: %w", err)
	}
	if len(listResp.ConferenceRecords) == 0 {
		return "", nil, fmt.Errorf("meeting code '%s' に該当する会議が見つかりませんでした", meetingCode)
	}

	// Step 2 & 3: Search through records (latest first) for one with non-empty transcript entries
	var allEntries []*meet.TranscriptEntry
	var conferenceRecord *meet.ConferenceRecord
	for i := len(listResp.ConferenceRecords) - 1; i >= 0; i-- {
		record := listResp.ConferenceRecords[i]
		tResp, tErr := meetService.ConferenceRecords.Transcripts.List(record.Name).Do()
		if tErr != nil || len(tResp.Transcripts) == 0 {
			continue
		}

		// Try to fetch entries for this transcript
		transcript := tResp.Transcripts[0]
		var entries []*meet.TranscriptEntry
		pageToken := ""
		for {
			call := meetService.ConferenceRecords.Transcripts.Entries.List(transcript.Name)
			if pageToken != "" {
				call = call.PageToken(pageToken)
			}
			resp, err := call.Do()
			if err != nil {
				break
			}
			entries = append(entries, resp.TranscriptEntries...)
			if resp.NextPageToken == "" {
				break
			}
			pageToken = resp.NextPageToken
		}

		if len(entries) > 0 {
			allEntries = entries
			conferenceRecord = record
			break
		}
		// Entries empty for this record, try next one
	}

	if len(allEntries) == 0 {
		return "", nil, fmt.Errorf("文字起こしエントリが見つかりませんでした（十分な長さの会話が必要です）")
	}

	// Combine entries
	var builder strings.Builder
	for i, entry := range allEntries {
		if entry.Participant != "" {
			parts := strings.Split(entry.Participant, "/")
			if len(parts) > 0 {
				builder.WriteString(fmt.Sprintf("[%s]: ", parts[len(parts)-1]))
			}
		}
		builder.WriteString(entry.Text)
		if i < len(allEntries)-1 {
			builder.WriteString("\n")
		}
	}

	meta := &transcriptMeta{
		EntryCount: len(allEntries),
	}
	if conferenceRecord.StartTime != "" {
		meta.StartTime = conferenceRecord.StartTime
	}

	return builder.String(), meta, nil
}

// Helper functions

func getToken(r *http.Request) *oauth2.Token {
	cookie, err := r.Cookie("session_id")
	if err != nil {
		return nil
	}
	sessionsMu.RLock()
	defer sessionsMu.RUnlock()
	return sessions[cookie.Value]
}

func generateSessionID() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		log.Fatal("failed to generate session ID")
	}
	return hex.EncodeToString(b)
}

func renderTemplate(w http.ResponseWriter, data pageData) {
	if err := tmpl.ExecuteTemplate(w, "index.html", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func renderPage(w http.ResponseWriter, data pageData) {
	renderTemplate(w, data)
}

func renderError(w http.ResponseWriter, msg string) {
	renderPage(w, pageData{Error: msg})
}
