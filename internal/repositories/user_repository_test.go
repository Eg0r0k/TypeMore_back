package repositories_test

import (
	"testing"
	"time"
	"typeMore/internal/repositories"

	"github.com/google/uuid"
	"gopkg.in/data-dog/go-sqlmock.v2"
)

func TestUserRepository_GetUserByUsername(t *testing.T) {

	db, mock, err := sqlmock.New()
	if err != nil {
			t.Fatalf("failed to create mock database: %v", err)
	}
	defer db.Close()

	repo := repositories.NewUserRepository(db)

	userID := uuid.New()
	username := "testuser"
	mock.ExpectQuery("SELECT id, username, email, is_banned, config, password, created_at, updated_at, last_in, last_out, registration_date").
			WithArgs(username).
			WillReturnRows(sqlmock.NewRows([]string{"id", "username", "email", "is_banned", "config", "password", "created_at", "updated_at", "last_in", "last_out", "registration_date"}).
					AddRow(userID, username, "test@example.com", false, "{}", "hashedpassword", time.Now(), time.Now(), nil, nil, nil))

	
	mock.ExpectQuery("SELECT r.name FROM user_roles ur JOIN roles r ON ur.role_id = r.id WHERE ur.user_id = \\$1").
			WithArgs(userID).
			WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("admin").AddRow("editor"))


	user, err := repo.GetUserByUsername(username)
	if err != nil {
			t.Fatalf("expected no error, got %v", err)
	}


	if user.UserId != userID || user.Username != username {
			t.Errorf("expected user id %v and username %s, got %v and %s", userID, username, user.UserId, user.Username)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("there were unfulfilled expectations: %s", err)
	}
}

func TestUserRepository_GetUserByID(t *testing.T) {
	db,mock,err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to create mock database: %v", err)
	}
	defer db.Close()
	repo:= repositories.NewUserRepository(db)

	userID := uuid.New()

	mock.ExpectQuery("SELECT id, username, email, is_banned, config, password, created_at, updated_at, last_in, last_out, registration_date").
	WithArgs(userID).
	WillReturnRows(sqlmock.NewRows([]string{"id", "username", "email", "is_banned", "config", "password", "created_at", "updated_at", "last_in", "last_out", "registration_date"}).
			AddRow(userID, "testuser", "test@example.com", false, "{}", "hashedpassword", time.Now(), time.Now(), nil, nil, nil))

	mock.ExpectQuery("SELECT r.name FROM user_roles ur JOIN roles r ON ur.role_id = r.id WHERE ur.user_id = \\$1").
	WithArgs(userID).
	WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("admin").AddRow("editor"))

	user, err := repo.GetUserByID(userID)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)

	}
	if user.UserId != userID {
		t.Errorf("expected user id %v, got %v", userID, user.UserId)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("there were unfulfilled expectations: %s", err)
}
}

func TestUserRepository_IsUsernameTaken(t *testing.T) {

	db, mock, err := sqlmock.New()
	if err != nil {
			t.Fatalf("failed to create mock database: %v", err)
	}
	defer db.Close()

	repo := repositories.NewUserRepository(db)

	username := "testuser"


	mock.ExpectQuery("SELECT EXISTS\\(SELECT 1 FROM users WHERE username = \\$1\\)").
			WithArgs(username).
			WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true)) 


	exists, err := repo.IsUsernameTaken(username)
	if err != nil {
			t.Fatalf("expected no error, got %v", err)
	}
	if !exists {
			t.Errorf("expected username %s to be taken, got %v", username, exists)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("there were unfulfilled expectations: %s", err)
	}
}


func TestUserRepository_IsEmailTaken(t *testing.T) {

	db, mock, err := sqlmock.New()
	if err != nil {
			t.Fatalf("failed to create mock database: %v", err)
	}
	defer db.Close()

	repo := repositories.NewUserRepository(db)

	email := "test@example.com"

	mock.ExpectQuery("SELECT EXISTS\\(SELECT 1 FROM users WHERE email = \\$1\\)").
			WithArgs(email).
			WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false)) 


	exists, err := repo.IsEmailTaken(email)
	if err != nil {
			t.Fatalf("expected no error, got %v", err)
	}
	if exists {
			t.Errorf("expected email %s to be available, got %v", email, exists)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("there were unfulfilled expectations: %s", err)
	}
}