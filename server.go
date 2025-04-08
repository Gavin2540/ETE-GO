package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

var db *gorm.DB
var mutex = &sync.Mutex{} // For concurrency control

// WebSocket upgrader
var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

// WebSocket clients map
var clients = make(map[*websocket.Conn]bool)
var clientsMutex sync.Mutex

// Movie struct
type Movie struct {
	ID       uint   `json:"id" gorm:"primaryKey;autoIncrement"`
	Title    string `json:"title"`
	Duration int    `json:"duration"`
	Showtime string `json:"showtime"`
	Poster   string `json:"poster"`
}

// Seat struct
type Seat struct {
	ID         uint   `json:"id" gorm:"primaryKey;autoIncrement"`
	MovieID    uint   `json:"movie_id"`
	SeatNumber string `json:"seat_number"`
	IsBooked   bool   `json:"is_booked"`
}

// Booking struct
type Booking struct {
	ID       uint   `json:"id" gorm:"primaryKey;autoIncrement"`
	UserName string `json:"user_name"`
	MovieID  uint   `json:"movie_id"`
	SeatID   uint   `json:"seat_id"`
}

// Rate limiter struct
type RateLimiter struct {
	visitors map[string]*visitor
	mu       sync.Mutex
	rate     time.Duration
	burst    int
}

type visitor struct {
	limiter  chan struct{}
	lastSeen time.Time
}

func NewRateLimiter(rate time.Duration, burst int) *RateLimiter {
	return &RateLimiter{
		visitors: make(map[string]*visitor),
		rate:     rate,
		burst:    burst,
	}
}

func (rl *RateLimiter) getVisitor(ip string) *visitor {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	v, exists := rl.visitors[ip]
	if !exists {
		v = &visitor{
			limiter:  make(chan struct{}, rl.burst),
			lastSeen: time.Now(),
		}
		rl.visitors[ip] = v
	}
	v.lastSeen = time.Now()
	return v
}

func (rl *RateLimiter) cleanupVisitors() {
	for {
		time.Sleep(time.Minute)
		rl.mu.Lock()
		for ip, v := range rl.visitors {
			if time.Since(v.lastSeen) > 3*time.Minute {
				delete(rl.visitors, ip)
			}
		}
		rl.mu.Unlock()
	}
}

func (rl *RateLimiter) Limit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := r.RemoteAddr
		v := rl.getVisitor(ip)

		select {
		case v.limiter <- struct{}{}:
			defer func() { <-v.limiter }()
			next.ServeHTTP(w, r)
		default:
			http.Error(w, "Too many requests", http.StatusTooManyRequests)
		}
	})
}

// Initialize Database
func initDB() {
	var err error
	dsn := "root:gavinrothu123@tcp(127.0.0.1:3306)/cinema_db?charset=utf8mb4&parseTime=True&loc=Local"

	log.Printf("Connecting to database with DSN: %s", dsn)

	// Add retry logic for database connection
	maxRetries := 5
	for i := 0; i < maxRetries; i++ {
		db, err = gorm.Open(mysql.Open(dsn), &gorm.Config{})
		if err == nil {
			break
		}
		log.Printf("Failed to connect to database (attempt %d/%d): %v", i+1, maxRetries, err)
		time.Sleep(5 * time.Second)
	}

	if err != nil {
		log.Fatal("Failed to connect to database after retries:", err)
	}

	log.Println("Successfully connected to database")

	// AutoMigrate tables
	err = db.AutoMigrate(&Movie{}, &Seat{}, &Booking{})
	if err != nil {
		log.Fatal("Failed to migrate database:", err)
	}
	log.Println("Database migration completed")

	// Check if seats exist for each movie
	var movies []Movie
	if err := db.Find(&movies).Error; err != nil {
		log.Fatal("Failed to fetch movies:", err)
	}

	for _, movie := range movies {
		var seatCount int64
		if err := db.Model(&Seat{}).Where("movie_id = ?", movie.ID).Count(&seatCount).Error; err != nil {
			log.Fatal("Failed to count seats:", err)
		}

		if seatCount == 0 {
			log.Printf("Creating seats for movie %s (ID: %d)", movie.Title, movie.ID)
			createSeatsForMovie(movie.ID)
		}
	}
}

// Create seats for a movie
func createSeatsForMovie(movieID uint) {
	tx := db.Begin()

	// Create 50 seats (5 rows × 10 seats)
	for row := 'A'; row <= 'E'; row++ {
		for seat := 1; seat <= 10; seat++ {
			seatNumber := fmt.Sprintf("%c%d", row, seat)
			if err := tx.Create(&Seat{
				MovieID:    movieID,
				SeatNumber: seatNumber,
				IsBooked:   false,
			}).Error; err != nil {
				tx.Rollback()
				log.Printf("Error creating seat %s for movie %d: %v", seatNumber, movieID, err)
				return
			}
		}
	}

	tx.Commit()
	log.Printf("Successfully created seats for movie %d", movieID)
}

// Fetch movies
func getMovies(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	var movies []Movie
	result := db.Find(&movies)
	if result.Error != nil {
		log.Printf("Error fetching movies: %v", result.Error)
		http.Error(w, "Error fetching movies", http.StatusInternalServerError)
		return
	}

	log.Printf("Found %d movies", len(movies))
	json.NewEncoder(w).Encode(movies)
}

// Fetch seats for a movie
func getSeats(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	movieID := mux.Vars(r)["id"]
	var seats []Seat

	if err := db.Where("movie_id = ?", movieID).Order("seat_number").Find(&seats).Error; err != nil {
		log.Printf("Error fetching seats for movie %s: %v", movieID, err)
		http.Error(w, "Error fetching seats", http.StatusInternalServerError)
		return
	}

	log.Printf("Found %d seats for movie %s", len(seats), movieID)
	json.NewEncoder(w).Encode(seats)
}

// Broadcast message to all connected clients
func broadcastMessage(message interface{}) {
	clientsMutex.Lock()
	defer clientsMutex.Unlock()

	for client := range clients {
		err := client.WriteJSON(message)
		if err != nil {
			log.Printf("Error broadcasting message: %v", err)
			client.Close()
			delete(clients, client)
		}
	}
}

// WebSocket handler
func handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("Error upgrading connection: %v", err)
		return
	}
	defer conn.Close()

	// Add client to the map
	clientsMutex.Lock()
	clients[conn] = true
	clientsMutex.Unlock()

	// Remove client when they disconnect
	defer func() {
		clientsMutex.Lock()
		delete(clients, conn)
		clientsMutex.Unlock()
	}()

	// Keep connection alive
	for {
		_, _, err := conn.ReadMessage()
		if err != nil {
			break
		}
	}
}

// BookingRequest struct
type BookingRequest struct {
	UserName  string `json:"user_name"`
	UserEmail string `json:"user_email"`
	MovieID   uint   `json:"movie_id"`
	SeatIDs   []uint `json:"seat_ids"`
}

// Modified bookSeat function to handle multiple seats
func bookSeat(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	var bookingReq BookingRequest
	if err := json.NewDecoder(r.Body).Decode(&bookingReq); err != nil {
		log.Printf("Error decoding booking request: %v", err)
		http.Error(w, "Invalid input", http.StatusBadRequest)
		return
	}

	log.Printf("Received booking request: %+v", bookingReq)

	// Lock for concurrency
	mutex.Lock()
	defer mutex.Unlock()

	tx := db.Begin()

	var bookings []Booking
	for _, seatID := range bookingReq.SeatIDs {
		// Check seat availability
		var seat Seat
		if err := tx.First(&seat, seatID).Error; err != nil {
			tx.Rollback()
			log.Printf("Error finding seat %d: %v", seatID, err)
			http.Error(w, "Seat not found", http.StatusNotFound)
			return
		}

		if seat.IsBooked {
			tx.Rollback()
			log.Printf("Seat %d is already booked", seatID)
			http.Error(w, "One or more seats are already booked", http.StatusConflict)
			return
		}

		// Mark seat as booked
		if err := tx.Model(&seat).Update("is_booked", true).Error; err != nil {
			tx.Rollback()
			log.Printf("Error updating seat %d: %v", seatID, err)
			http.Error(w, "Failed to book seat", http.StatusInternalServerError)
			return
		}

		// Create booking record
		booking := Booking{
			UserName: bookingReq.UserName,
			MovieID:  bookingReq.MovieID,
			SeatID:   seatID,
		}
		if err := tx.Create(&booking).Error; err != nil {
			tx.Rollback()
			log.Printf("Error creating booking for seat %d: %v", seatID, err)
			http.Error(w, "Failed to create booking", http.StatusInternalServerError)
			return
		}
		bookings = append(bookings, booking)
	}

	tx.Commit()
	log.Printf("Successfully created %d bookings for user %s", len(bookings), bookingReq.UserName)

	// Broadcast seat update to all connected clients
	broadcastMessage(map[string]interface{}{
		"type":     "seat_update",
		"movie_id": bookingReq.MovieID,
	})

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(bookings)
}

// Middleware for CORS
func enableCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Add movie
func addMovie(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	var movie Movie
	if err := json.NewDecoder(r.Body).Decode(&movie); err != nil {
		log.Printf("Error decoding movie data: %v", err)
		http.Error(w, "Invalid input", http.StatusBadRequest)
		return
	}

	log.Printf("Attempting to add movie: %+v", movie)

	// Create seats for the movie
	tx := db.Begin()

	if err := tx.Create(&movie).Error; err != nil {
		tx.Rollback()
		log.Printf("Error creating movie: %v", err)
		http.Error(w, "Failed to create movie", http.StatusInternalServerError)
		return
	}

	// Create 50 seats for the movie (5 rows x 10 seats)
	for row := 'A'; row <= 'E'; row++ {
		for seat := 1; seat <= 10; seat++ {
			seatNumber := fmt.Sprintf("%c%d", row, seat)
			if err := tx.Create(&Seat{
				MovieID:    movie.ID,
				SeatNumber: seatNumber,
				IsBooked:   false,
			}).Error; err != nil {
				tx.Rollback()
				log.Printf("Error creating seat %s: %v", seatNumber, err)
				http.Error(w, "Failed to create seats", http.StatusInternalServerError)
				return
			}
		}
	}

	tx.Commit()
	log.Printf("Successfully added movie with ID %d and created seats", movie.ID)

	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(movie)
}

// Delete movie
func deleteMovie(w http.ResponseWriter, r *http.Request) {
	movieID := mux.Vars(r)["id"]

	// Start transaction
	tx := db.Begin()

	// Delete associated seats
	if err := tx.Where("movie_id = ?", movieID).Delete(&Seat{}).Error; err != nil {
		tx.Rollback()
		http.Error(w, "Failed to delete associated seats", http.StatusInternalServerError)
		return
	}

	// Delete movie
	if err := tx.Delete(&Movie{}, movieID).Error; err != nil {
		tx.Rollback()
		http.Error(w, "Failed to delete movie", http.StatusInternalServerError)
		return
	}

	tx.Commit()
	w.WriteHeader(http.StatusOK)
}

func main() {
	log.Println("Starting server initialization...")

	initDB()
	router := mux.NewRouter()

	// Initialize rate limiter
	limiter := NewRateLimiter(time.Minute, 10)
	go limiter.cleanupVisitors()

	// Create posters directory if it doesn't exist
	err := os.MkdirAll(filepath.Join("static", "posters"), 0755)
	if err != nil {
		log.Printf("Error creating posters directory: %v", err)
	}

	// API routes
	router.HandleFunc("/api/movies", getMovies).Methods("GET", "OPTIONS")
	router.HandleFunc("/api/movies", addMovie).Methods("POST")
	router.HandleFunc("/api/movies/{id}", deleteMovie).Methods("DELETE")
	router.HandleFunc("/api/seats/{id}", getSeats).Methods("GET")
	router.HandleFunc("/api/book", bookSeat).Methods("POST")
	router.HandleFunc("/ws", handleWebSocket)

	// Serve static files
	router.PathPrefix("/static/").Handler(http.StripPrefix("/static/", http.FileServer(http.Dir("./static"))))
	router.PathPrefix("/").Handler(http.FileServer(http.Dir("./")))

	// Enable CORS
	corsRouter := enableCORS(router)
	http.Handle("/", corsRouter)

	log.Println("Server starting on port 8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}
