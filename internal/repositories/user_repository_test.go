package repositories_test

import (
	"context"
	"testing"
	"time"
	"typeMore/internal/models"
	"typeMore/internal/repositories"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
)

func TestCreateUser(t *testing.T) {

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to open sqlmock: %v", err)
	}
	defer db.Close()

	repo := repositories.NewUserRepository(db)

	userID := uuid.New()
	user := &models.User{
		UserId:   userID,
		Username: "testuser",
		Email:    "test@example.com",
		Password: []byte("password"),
		IsBanned: false,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
		Roles:    []models.Role{models.UserRole},
	}


	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO users").
		WithArgs(user.UserId, user.Username, user.Email, user.Password, user.IsBanned, user.Config,
			user.CreatedAt, user.UpdatedAt, user.LastIn, user.LastOut, user.RegistrationDate).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec("INSERT INTO user_roles").
		WithArgs(user.UserId, user.Roles[0]).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()


	err = repo.CreateUser(user)
	assert.NoError(t, err)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("there were unfulfilled expectations: %s", err)
	}
}

func TestGetUserByID(t *testing.T) {
    db, mock, err := sqlmock.New()
    if err != nil {
        t.Fatalf("failed to open sqlmock: %v", err)
    }
    defer db.Close()

    repo := repositories.NewUserRepository(db)

    userID := uuid.New()
    now := time.Now()

    mock.ExpectQuery("SELECT id, username, email, is_banned, config, password, created_at, updated_at, last_in, last_out, registration_date").
        WithArgs(userID).
        WillReturnRows(sqlmock.NewRows([]string{"id", "username", "email", "is_banned", "config", "password", "created_at", "updated_at", "last_in", "last_out", "registration_date"}).
            AddRow(userID, "testuser", "test@example.com", false, "", []byte("password"), now, now, nil, nil, nil))


    mock.ExpectQuery("SELECT r.name").
        WithArgs(userID).
        WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("admin"))


    user, err := repo.GetUserByID(context.Background(), userID)
    assert.NoError(t, err)
    assert.NotNil(t, user)
    assert.Equal(t, user.Username, "testuser")
    assert.Equal(t, len(user.Roles), 1) 
    assert.Equal(t, user.Roles[0], models.AdminRole) 

  
    if err := mock.ExpectationsWereMet(); err != nil {
        t.Errorf("there were unfulfilled expectations: %s", err)
    }
}

func TestDeleteUser(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to open sqlmock: %v", err)
	}
	defer db.Close()

	repo := repositories.NewUserRepository(db)

	userID := uuid.New()
	mock.ExpectExec("DELETE FROM users").
		WithArgs(userID).
		WillReturnResult(sqlmock.NewResult(1, 1))

	err = repo.DeleteUser(context.Background(), userID)
	assert.NoError(t, err)


	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("there were unfulfilled expectations: %s", err)
	}
}

func TestDeleteRefreshToken(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to open sqlmock: %v", err)
	}
	defer db.Close()

	repo := repositories.NewUserRepository(db)

	userID := uuid.New()
	token := "sample_token"
	mock.ExpectExec("DELETE FROM refresh_tokens").
		WithArgs(userID, token).
		WillReturnResult(sqlmock.NewResult(1, 1))


	err = repo.DeleteRefreshToken(context.Background(), userID, token)
	assert.NoError(t, err)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("there were unfulfilled expectations: %s", err)
	}
}

func TestIsUsernameTaken(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to open sqlmock: %v", err)
	}
	defer db.Close()

	repo := repositories.NewUserRepository(db)

	username := "testuser"
	mock.ExpectQuery("SELECT EXISTS").
		WithArgs(username).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))


	exists, err := repo.IsUsernameTaken(context.Background(), username)
	assert.NoError(t, err)
	assert.True(t, exists)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("there were unfulfilled expectations: %s", err)
	}
}

func TestIsEmailTaken(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to open sqlmock: %v", err)
	}
	defer db.Close()

	repo := repositories.NewUserRepository(db)

	email := "test@example.com"
	mock.ExpectQuery("SELECT EXISTS").
		WithArgs(email).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))


	exists, err := repo.IsEmailTaken(context.Background(), email)
	assert.NoError(t, err)
	assert.False(t, exists)


	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("there were unfulfilled expectations: %s", err)
	}
}
