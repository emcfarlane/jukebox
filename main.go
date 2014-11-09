package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket" // Websockets
)

var (
	debug    = flag.Bool("debug", false, "Debug flag")
	upgrader = websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
	}
)

// Error wrapper
func errorHandler(f func(w http.ResponseWriter, r *http.Request) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := f(w, r); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			log.Println("error handling %q: %v", r.RequestURI, err)
		}
	}
}

// Single file serving
func (s *Server) sServe(pattern string, filename string) {
	http.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, filename)
	})
}

type Song struct {
	Name  string
	Score int
}

type State struct {
	Address string
	Songs   []Song
	Playing string
}

type Message struct {
	Command string
	Song    Song
	Time    int
}

type Server struct {
	songLock    *sync.Mutex
	songMap     map[string]int
	songList    []Song
	songPlaying *Message

	sockLock  *sync.Mutex
	sockUsers []*websocket.Conn

	addrs string
	tmpl  *template.Template
}

func (s *Server) plus(song Song) {
	s.songUpdate(song, +1)
}
func (s *Server) minus(song Song) {
	s.songUpdate(song, -1)
}

func (s *Server) songUpdate(song Song, i int) {
	s.songLock.Lock()
	defer s.songLock.Unlock()

	s.songMap[song.Name] = s.songMap[song.Name] + i
	song.Score = s.songMap[song.Name]

	msg := &Message{
		Command: "update",
		Song:    song,
	}

	log.Println(s.sockUsers)
	s.sockWriteLoop(msg)
}

func makeTimestamp() int64 {
	return time.Now().UnixNano() / int64(time.Millisecond)
}

func (s *Server) next(song Song) {
	s.songLock.Lock()
	defer s.songLock.Unlock()
	if song.Name != s.songPlaying.Song.Name && s.songPlaying.Song.Name != "" {
		log.Println("Error: Should not call next")
		return
	}
	// Find next song
	var topSong string

	// Random first song
	for key, _ := range s.songMap {
		topSong = key
		break
	}

	// Generate next values
	for key, value := range s.songMap {
		if value >= s.songMap[topSong] {
			topSong = key
		}
	}
	song.Name = topSong

	// Update
	s.songMap[song.Name] = 0
	song.Score = s.songMap[song.Name]
	msg := &Message{
		Command: "play",
		Song:    song,
		Time:    int(makeTimestamp()),
	}

	log.Println("Now Playing: ", song.Name)
	s.songPlaying = msg
	s.sockWriteLoop(msg)
}

func (s *Server) sockPopUser(c *websocket.Conn) {
	s.sockLock.Lock()
	defer s.sockLock.Unlock()
	for i := range s.sockUsers {
		if s.sockUsers[i] == c {
			s.sockUsers = append(s.sockUsers[:i], s.sockUsers[i+1:]...)
			break
		}
	}
}

// Sock read loop
func (s *Server) sockReadLoop(c *websocket.Conn) {
	var msg Message
	for {
		if err := websocket.ReadJSON(c, &msg); err != nil {
			log.Println("SOCKET ERROR!")
			log.Println(msg)
			s.sockPopUser(c)
			c.Close()
			break
		}
		log.Println("sockReadLoop: Commad: ", msg.Command)
		switch msg.Command {
		case "plus":
			s.plus(msg.Song)
		case "minus":
			s.minus(msg.Song)
		case "next":
			if msg.Song.Name != s.songPlaying.Song.Name && s.songPlaying.Song.Name != "" {
				log.Println("New Stream")
				log.Println(msg.Song.Name)
				s.sockLock.Lock()
				websocket.WriteJSON(c, s.songPlaying)
				s.sockLock.Unlock()
				log.Println(s.songPlaying.Command)
			} else {
				log.Println("New song")
				s.next(msg.Song)
			}
		default:
			log.Println("sockReadLoop: Command unknown, ", msg.Command)
		}
	}
}

// Sock write
func (s *Server) sockWriteLoop(data interface{}) {
	s.sockLock.Lock()
	defer s.sockLock.Unlock()
	for i := range s.sockUsers {
		c := s.sockUsers[i]
		if err := websocket.WriteJSON(c, data); err != nil {
			log.Println("sockWriteLoop: Error wrting json, ", err)
		}
	}
}

// Websocket handles
func (s *Server) sock(w http.ResponseWriter, r *http.Request) error {
	c, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return err
	}
	//defer c.Close()

	// Log
	log.Println("sock: Got new user!")

	// Read
	go s.sockReadLoop(c)

	// Write
	s.sockLock.Lock()
	defer s.sockLock.Unlock()
	s.sockUsers = append(s.sockUsers, c)

	return nil
}

// Song generation
var isAudio = map[string]bool{
	".mp3": true,
	".ogg": true,
	".wav": true,
}

func (s *Server) songGen() error {
	s.songLock.Lock()
	defer s.songLock.Unlock()

	// Folders to serch for music... Need to expand to many files
	files, err := ioutil.ReadDir("Music")
	if err != nil {
		return err
	}

	// Add files to library
	for i := range files {
		if !files[i].IsDir() && isAudio[strings.ToLower(filepath.Ext(files[i].Name()))] {
			s.songMap[files[i].Name()] = 0
		}
	}
	return nil
}

type Dukebox struct {
	Address string
	Songs   []Song
}

func (s *Server) pageGen() (*bytes.Reader, error) {
	s.songLock.Lock()
	defer s.songLock.Unlock()

	var songs []Song
	for key, value := range s.songMap {
		songs = append(songs, Song{Name: key, Score: value})
	}

	data := &Dukebox{
		Address: s.addrs,
		Songs:   songs,
	}

	b := new(bytes.Buffer)
	err := s.tmpl.ExecuteTemplate(b, "base.html", data)
	if err != nil {
		return nil, nil
	}
	return bytes.NewReader(b.Bytes()), nil
}

// Http handles
func (s *Server) client(w http.ResponseWriter, r *http.Request) error {
	content, err := s.pageGen()
	http.ServeContent(w, r, ".html", time.Now(), content)
	return err
}
func (s *Server) audio(w http.ResponseWriter, r *http.Request) error {
	path, err := url.QueryUnescape(strings.TrimPrefix(r.URL.String(), "/audio"))
	if err != nil {
		return err
	}
	f, err := os.Open("Music" + path)
	defer f.Close()
	if err != nil {
		return err
	}
	log.Println("Audio Request!")

	//w.Header().Set("X-Content-Duration", string(20))
	//w.WriteHeader(http.StatusPartialContent)

	http.ServeContent(w, r, "", time.Now(), f)
	//http.ServeFile(w, r, "Music"+path)
	return nil
}

func main() {
	name, err := os.Hostname()
	if err != nil {
		fmt.Printf("Oops: %v\n", err)
		return
	}
	addrs, err := net.LookupHost(name)
	if err != nil {
		fmt.Printf("Oops: %v\n", err)
		return
	}

	tmpl, err := template.ParseFiles("base.html")
	if err != nil {
		fmt.Printf("Oops: %v\n", err)
		return
	}

	// Server
	s := &Server{
		songLock:    &sync.Mutex{},
		songMap:     make(map[string]int),
		songPlaying: &Message{Song: Song{Name: ""}},

		sockLock:  &sync.Mutex{},
		sockUsers: []*websocket.Conn{},

		addrs: addrs[0] + ":8000",
		tmpl:  tmpl,
	}

	// Generate songs
	if err := s.songGen(); err != nil {
		log.Println(err)
	}
	log.Println(s.songMap)

	// Http handles
	http.HandleFunc("/", errorHandler(s.client))
	http.HandleFunc("/audio/", errorHandler(s.audio))

	http.HandleFunc("/sock", errorHandler(s.sock))

	s.sServe("/list.min.js", "list.min.js")
	s.sServe("/style.css", "style.css")

	msg := &Message{
		Command: "play",
		Song:    Song{Name: "sup", Score: 0},
		Time:    int(makeTimestamp()),
	}

	b, _ := json.Marshal(msg)
	fmt.Println(string(b))

	// Run
	log.Println("Running: ", s.addrs)
	err = http.ListenAndServe(":8000", nil)
	if err != nil {
		log.Fatal("ListenAndServe: ", err)
	}
}
