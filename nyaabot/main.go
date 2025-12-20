package main

import (
	_ "embed"
	"encoding/json"
	//"fmt"
	"html/template"
	"log"
	"math/rand"
	"net/http"
	"os"
	//"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/irevenko/go-nyaa/nyaa"
	tele "gopkg.in/telebot.v3"
)

// --- BAGIAN 1: HTML EMBED ---
//go:embed index.html
var indexHTML string

// --- BAGIAN 2: STRUKTUR DATA DOWNLOAD ---
type DownloadStatus struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	Progress float64 `json:"progress"` // 0 - 100
	Status   string  `json:"status"`   // "pending", "downloading", "done", "error"
	FileURL  string  `json:"file_url"` // Link download final
	ErrorMsg string  `json:"error_msg"`
}

// Penyimpanan sementara download yg sedang berjalan
var (
	activeDownloads = make(map[string]*DownloadStatus)
	mutex           = &sync.Mutex{} // Supaya aman diakses banyak user
)

func main() {
	// 1. Buat Folder Downloads
	os.Mkdir("downloads", 0755)

	// 2. Setup Bot Telegram
	pref := tele.Settings{
		Token:  "8360602898:AAHZ_DsWmQ_IzMaiBhCzdddFNRwPuwwLt-A", // <--- GANTI TOKEN
		Poller: &tele.LongPoller{Timeout: 10 * time.Second},
	}

	b, err := tele.NewBot(pref)
	if err != nil {
		log.Fatal(err)
		return
	}

	// 3. HANDLER WEB SERVER
	
	// A. Halaman Utama
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		tmpl, _ := template.New("index").Parse(indexHTML)
		tmpl.Execute(w, nil)
	})

	// B. File Server (Untuk user ambil video yg sudah selesai)
	// User akses: /files/judul_anime.mp4
	fs := http.FileServer(http.Dir("./downloads"))
	http.Handle("/files/", http.StripPrefix("/files/", fs))

	// C. API: Search Nyaa
	http.HandleFunc("/api/search", handleSearch)

	// D. API: Start Download (Server side)
	http.HandleFunc("/api/start_download", handleStartDownload)

	// E. API: Cek Progress
	http.HandleFunc("/api/progress", handleCheckProgress)

	// Jalankan Server
	go func() {
		log.Println("🌐 Server jalan di port 8080...")
		if err := http.ListenAndServe(":8899", nil); err != nil {
			log.Fatal(err)
		}
	}()

	// 4. Setup Bot Menu
	menu := &tele.ReplyMarkup{ResizeKeyboard: true}
	// INGAT GANTI URL NGROK
	btnWebApp := menu.WebApp("🚀 Buka Nyaa App", &tele.WebApp{URL: "https://miniapp.hudacihuyy.qzz.io"})
	menu.Reply(menu.Row(btnWebApp))

	b.Handle("/start", func(c tele.Context) error {
		return c.Send("Halo! Klik tombol di bawah untuk membuka Nyaa Downloader.", menu)
	})

	log.Println("🤖 Bot siap!")
	b.Start()
}

// --- FUNGSI HANDLER ---

func handleSearch(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("q")
	opt := nyaa.SearchOptions{
		Provider: "nyaa", Query: query, Category: "anime", SortBy: "seeders",
	}
	res, _ := nyaa.Search(opt)
	if len(res) > 20 { res = res[:20] } // Limit 20
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(res)
}

func handleStartDownload(w http.ResponseWriter, r *http.Request) {
	torrentUrl := r.URL.Query().Get("url")
	
	// Buat ID unik acak
	downloadID := strconv.Itoa(rand.Intn(100000))
	
	// Simpan status awal
	ds := &DownloadStatus{ID: downloadID, Status: "pending", Progress: 0}
	mutex.Lock()
	activeDownloads[downloadID] = ds
	mutex.Unlock()

	// Jalankan download di Goroutine (Background)
	go processDownload(downloadID, torrentUrl)

	// Balas ke frontend dengan ID
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"id": downloadID})
}

func handleCheckProgress(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	
	mutex.Lock()
	ds, exists := activeDownloads[id]
	mutex.Unlock()

	if !exists {
		http.Error(w, "Not found", 404)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(ds)
}

// --- LOGIKA TORRENT ---

func processDownload(id, torrentUrl string) {
	// Helper update status
	updateStatus := func(status string, prog float64, msg string, fileUrl string) {
		mutex.Lock()
		defer mutex.Unlock()
		if d, ok := activeDownloads[id]; ok {
			d.Status = status
			d.Progress = prog
			d.ErrorMsg = msg
			d.FileURL = fileUrl
			if status == "downloading" { d.Name = msg } // Hack: msg used as name
		}
	}

	// 1. Download .torrent
	resp, err := http.Get(torrentUrl)
	if err != nil {
		updateStatus("error", 0, "Gagal konek Nyaa", "")
		return
	}
	defer resp.Body.Close()

	mi, err := metainfo.Load(resp.Body)
	if err != nil {
		updateStatus("error", 0, "Torrent Invalid", "")
		return
	}

	// 2. Setup Client (IPv6 Disabled)
	cfg := torrent.NewDefaultClientConfig()
	cfg.DataDir = "downloads"
	//cfg.DisableIPv6 = true // <--- FIX IPv6
	
	client, err := torrent.NewClient(cfg)
	if err != nil {
		updateStatus("error", 0, "Gagal start client", "")
		return
	}
	defer client.Close()

	t, _ := client.AddTorrent(mi)
	
	updateStatus("downloading", 0, "Mencari Metadata...", "")
	
	// Tunggu info
	select {
	case <-t.GotInfo():
	case <-time.After(60 * time.Second):
		updateStatus("error", 0, "Timeout Metadata (Sepi Seeders)", "")
		return
	}

	// Pilih file terbesar
	var targetFile *torrent.File
	var maxSz int64
	for _, f := range t.Files() {
		if f.Length() > maxSz {
			maxSz = f.Length()
			targetFile = f
		}
	}

	if targetFile == nil {
		updateStatus("error", 0, "Torrent kosong", "")
		return
	}

	// Update nama file
	mutex.Lock()
	activeDownloads[id].Name = targetFile.DisplayPath()
	mutex.Unlock()

	// 3. Start Download
	targetFile.Download()

	// Loop Monitoring
	for t.BytesCompleted() < t.Info().TotalLength() {
		pct := float64(t.BytesCompleted()) / float64(t.Info().TotalLength()) * 100
		
		updateStatus("downloading", pct, targetFile.DisplayPath(), "")
		
		time.Sleep(1 * time.Second)
		if t.BytesCompleted() >= t.Info().TotalLength() {
			break
		}
	}

	// 4. Selesai
	// Buat link download statis relative path
	finalLink := "/files/" + targetFile.Path()
	updateStatus("done", 100, targetFile.DisplayPath(), finalLink)
}
