package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/mux"
	"github.com/stretchr/testify/assert"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func setupTestDB() *gorm.DB {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		panic("failed to connect to test database")
	}
	db.AutoMigrate(&Movie{}, &Seat{}, &Booking{})
	return db
}

func TestGetMovies(t *testing.T) {
	// Setup
	db = setupTestDB()
	db.Create(&Movie{Title: "Test Movie", Duration: 120, Showtime: "14:00"})

	req, err := http.NewRequest("GET", "/api/movies", nil)
	assert.NoError(t, err)

	rr := httptest.NewRecorder()
	handler := http.HandlerFunc(getMovies)
	handler.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusOK, rr.Code)

	var movies []Movie
	err = json.Unmarshal(rr.Body.Bytes(), &movies)
	assert.NoError(t, err)
	assert.Equal(t, 1, len(movies))
	assert.Equal(t, "Test Movie", movies[0].Title)
}

func TestGetSeats(t *testing.T) {
	// Setup
	db = setupTestDB()
	movie := Movie{Title: "Test Movie"}
	db.Create(&movie)
	db.Create(&Seat{MovieID: movie.ID, SeatNumber: "A1", IsBooked: false})

	req, err := http.NewRequest("GET", "/api/seats/1", nil)
	assert.NoError(t, err)

	rr := httptest.NewRecorder()
	router := mux.NewRouter()
	router.HandleFunc("/api/seats/{id}", getSeats)
	router.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusOK, rr.Code)

	var seats []Seat
	err = json.Unmarshal(rr.Body.Bytes(), &seats)
	assert.NoError(t, err)
	assert.Equal(t, 1, len(seats))
	assert.Equal(t, "A1", seats[0].SeatNumber)
}

func TestBookSeat(t *testing.T) {
	// Setup
	db = setupTestDB()
	movie := Movie{Title: "Test Movie"}
	db.Create(&movie)
	seat := Seat{MovieID: movie.ID, SeatNumber: "A1", IsBooked: false}
	db.Create(&seat)

	booking := Booking{
		UserName: "Test User",
		MovieID:  movie.ID,
		SeatID:   seat.ID,
	}
	bookingJSON, _ := json.Marshal(booking)

	req, err := http.NewRequest("POST", "/api/book", bytes.NewBuffer(bookingJSON))
	assert.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")

	rr := httptest.NewRecorder()
	handler := http.HandlerFunc(bookSeat)
	handler.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusOK, rr.Code)

	var result Booking
	err = json.Unmarshal(rr.Body.Bytes(), &result)
	assert.NoError(t, err)
	assert.Equal(t, "Test User", result.UserName)

	// Verify seat is booked
	var updatedSeat Seat
	db.First(&updatedSeat, seat.ID)
	assert.True(t, updatedSeat.IsBooked)
}

func TestBookSeatAlreadyBooked(t *testing.T) {
	// Setup
	db = setupTestDB()
	movie := Movie{Title: "Test Movie"}
	db.Create(&movie)
	seat := Seat{MovieID: movie.ID, SeatNumber: "A1", IsBooked: true}
	db.Create(&seat)

	booking := Booking{
		UserName: "Test User",
		MovieID:  movie.ID,
		SeatID:   seat.ID,
	}
	bookingJSON, _ := json.Marshal(booking)

	req, err := http.NewRequest("POST", "/api/book", bytes.NewBuffer(bookingJSON))
	assert.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")

	rr := httptest.NewRecorder()
	handler := http.HandlerFunc(bookSeat)
	handler.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusConflict, rr.Code)
}

func TestBookSeatInvalidInput(t *testing.T) {
	// Setup
	db = setupTestDB()

	req, err := http.NewRequest("POST", "/api/book", bytes.NewBuffer([]byte("invalid json")))
	assert.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")

	rr := httptest.NewRecorder()
	handler := http.HandlerFunc(bookSeat)
	handler.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusBadRequest, rr.Code)
}
