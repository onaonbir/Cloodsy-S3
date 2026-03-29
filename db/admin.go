package db

import (
	"database/sql"
	"time"
)

type AdminCredential struct {
	ID           int64
	Username     string
	PasswordHash string
	CreatedAt    time.Time
}

func (d *DB) CreateAdmin(username, passwordHash string) (*AdminCredential, error) {
	var admin *AdminCredential
	err := d.withRetry(func() error {
		res, err := d.writer.Exec(
			"INSERT INTO admin_credentials (username, password_hash) VALUES (?, ?)",
			username, passwordHash,
		)
		if err != nil {
			return err
		}
		id, _ := res.LastInsertId()
		admin = &AdminCredential{
			ID:           id,
			Username:     username,
			PasswordHash: passwordHash,
			CreatedAt:    time.Now(),
		}
		return nil
	})
	return admin, err
}

func (d *DB) GetAdmin(username string) (*AdminCredential, error) {
	a := &AdminCredential{}
	err := d.reader.QueryRow(
		"SELECT id, username, password_hash, created_at FROM admin_credentials WHERE username = ?",
		username,
	).Scan(&a.ID, &a.Username, &a.PasswordHash, &a.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return a, err
}

func (d *DB) ListAdmins() ([]AdminCredential, error) {
	rows, err := d.reader.Query("SELECT id, username, password_hash, created_at FROM admin_credentials ORDER BY username")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var admins []AdminCredential
	for rows.Next() {
		var a AdminCredential
		if err := rows.Scan(&a.ID, &a.Username, &a.PasswordHash, &a.CreatedAt); err != nil {
			return nil, err
		}
		admins = append(admins, a)
	}
	return admins, rows.Err()
}

func (d *DB) DeleteAdmin(username string) error {
	return d.withRetry(func() error {
		_, err := d.writer.Exec("DELETE FROM admin_credentials WHERE username = ?", username)
		return err
	})
}

func (d *DB) UpdateAdminPassword(username, passwordHash string) error {
	return d.withRetry(func() error {
		_, err := d.writer.Exec("UPDATE admin_credentials SET password_hash = ? WHERE username = ?", passwordHash, username)
		return err
	})
}

func (d *DB) CountAdmins() (int, error) {
	var count int
	err := d.reader.QueryRow("SELECT COUNT(*) FROM admin_credentials").Scan(&count)
	return count, err
}

func (d *DB) AdminExists() (bool, error) {
	var count int
	err := d.reader.QueryRow("SELECT COUNT(*) FROM admin_credentials").Scan(&count)
	return count > 0, err
}
