package main

import (
	"crypto/rand"
	"database/sql"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

var db *sql.DB
var tmplIndex *template.Template
var tmplRows *template.Template

type Webhook struct {
	ID         int
	UserID     string
	Payload    string
	ReceivedAt time.Time
}

// A struct to pass data to our HTML template
type PageData struct {
	UserID string
	Host   string // Add this
}

func main() {
	initDB()
	defer db.Close()

	var err error
	tmplIndex, _ = template.ParseFiles("templates/index.html")
	tmplRows, err = template.ParseFiles("templates/rows.html")
	if err != nil {
		log.Fatal("Error parsing templates:", err)
	}

	port := ":8080"
	mux := http.NewServeMux()

	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.Dir("./static"))))

	// Root redirect
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
	})

	// UPDATED: Dashboard with Cookie login
	mux.HandleFunc("GET /dashboard", func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("hookdrop_uid")
		var userID string
		if err != nil {
			userID = generateUserID()
			newCookie := http.Cookie{
				Name:     "hookdrop_uid",
				Value:    userID,
				Path:     "/",
				MaxAge:   86400 * 30,
				HttpOnly: true,
			}
			http.SetCookie(w, &newCookie)
		} else {
			userID = cookie.Value
		}

		// Pass the Host (domain) to the template
		data := PageData{UserID: userID, Host: r.Host}
		tmplIndex.Execute(w, data)
	})

	mux.HandleFunc("GET /dashboard/rows", handleRows)
	mux.HandleFunc("POST /webhook/{id}", handleWebhook)
	mux.HandleFunc("POST /webhook/replay", handleReplay)
	mux.HandleFunc("POST /webhook/clear", handleClear)
	fmt.Printf("Starting server on http://localhost%s\n", port)
	log.Fatal(http.ListenAndServe(port, mux))

	// Use = instead of := because port is already declared!
	port = os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	// Ensure the port has a colon (e.g., ":8080" instead of "8080")
	if port[0] != ':' {
		port = ":" + port
	}

	fmt.Printf("Starting server on port %s\n", port)

	// Bind to 0.0.0.0 so Passenger can reach it
	log.Fatal(http.ListenAndServe("0.0.0.0"+port, mux))
}

func handleClear(w http.ResponseWriter, r *http.Request) {
	// Get the user from the cookie
	cookie, err := r.Cookie("hookdrop_uid")
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	userID := cookie.Value

	// Delete only THIS user's webhooks
	_, err = db.Exec("DELETE FROM webhooks WHERE user_id = ?", userID)
	if err != nil {
		http.Error(w, "Failed to clear webhooks", http.StatusInternalServerError)
		return
	}

	// Return an empty rows template so HTMX instantly clears the table
	tmplRows.Execute(w, nil)
}

func handleRows(w http.ResponseWriter, r *http.Request) {
	// Get the user ID from the cookie
	cookie, err := r.Cookie("hookdrop_uid")
	if err != nil {
		// If no cookie, return an empty table
		tmplRows.Execute(w, nil)
		return
	}
	userID := cookie.Value

	rows, err := db.Query("SELECT id, payload, received_at FROM webhooks WHERE user_id = ? ORDER BY id DESC", userID)
	if err != nil {
		http.Error(w, "Failed to query database", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var webhooks []Webhook
	for rows.Next() {
		var wh Webhook
		if err := rows.Scan(&wh.ID, &wh.Payload, &wh.ReceivedAt); err != nil {
			continue
		}
		webhooks = append(webhooks, wh)
	}

	if err := rows.Err(); err != nil {
		http.Error(w, "Error reading from database", http.StatusInternalServerError)
		return
	}

	tmplRows.Execute(w, webhooks)
}

// (Keep handleWebhook, handleReplay, initDB, and saveWebhook exactly as they are)
func handleWebhook(w http.ResponseWriter, r *http.Request) {
	userID := r.PathValue("id")
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read body", http.StatusInternalServerError)
		return
	}
	defer r.Body.Close()
	err = saveWebhook(userID, string(body))
	if err != nil {
		http.Error(w, "Failed to save webhook", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status": "success"}`))
}

func handleReplay(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := r.FormValue("id")
	targetURL := r.FormValue("target_url")
	var payload string
	err := db.QueryRow("SELECT payload FROM webhooks WHERE id = ?", id).Scan(&payload)
	if err != nil {
		http.Error(w, "Webhook not found", http.StatusNotFound)
		return
	}
	req, err := http.NewRequest("POST", targetURL, strings.NewReader(payload))
	if err != nil {
		http.Error(w, "Failed to create request", http.StatusInternalServerError)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		w.Write([]byte(`<span style="color: #ef4444; font-weight: bold;">Failed to connect</span>`))
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		w.Write([]byte(`<span style="color: #10b981; font-weight: bold;">Success (200)</span>`))
	} else {
		w.Write([]byte(fmt.Appendf(nil, `<span style="color: #f59e0b; font-weight: bold;">Error (%d)</span>`, resp.StatusCode)))
	}
}

func initDB() {
	var err error
	db, err = sql.Open("sqlite", "./data/hookdrop.db")
	if err != nil {
		log.Fatal("Failed to open database:", err)
	}
	query := `CREATE TABLE IF NOT EXISTS webhooks (id INTEGER PRIMARY KEY AUTOINCREMENT, user_id TEXT NOT NULL, payload TEXT NOT NULL, received_at DATETIME NOT NULL);`
	db.Exec(query)
}

func saveWebhook(userID string, payload string) error {
	query := `INSERT INTO webhooks (user_id, payload, received_at) VALUES (?, ?, ?)`
	_, err := db.Exec(query, userID, payload, time.Now())
	return err
}

// Generates a random 8-character string (e.g., "aB3x9LqP")
func generateUserID() string {
	const charset = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 8)
	rand.Read(b) // Fill 'b' with random bytes
	for i := range b {
		b[i] = charset[int(b[i])%len(charset)]
	}
	return string(b)
}
