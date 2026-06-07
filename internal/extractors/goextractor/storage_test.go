package goextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func TestExtractStorage_CreateTable(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"pkg/db/tables.go": `package db

const CreateUsersTable = ` + "`" + `
CREATE TABLE IF NOT EXISTS users (
    id INT AUTO_INCREMENT PRIMARY KEY,
    username VARCHAR(255) NOT NULL UNIQUE,
    email VARCHAR(255) NOT NULL UNIQUE
);` + "`" + `

const CreateOrdersTable = ` + "`" + `
CREATE TABLE orders (
    id INT AUTO_INCREMENT PRIMARY KEY,
    user_id INT NOT NULL
);` + "`" + `
`,
	})

	storage := findFactsByKind(ff, facts.KindStorage)

	foundUsers := false
	foundOrders := false
	for _, s := range storage {
		if s.Name == "users" && s.Props["storage_kind"] == "table" && s.Props["operation"] == "CREATE" {
			foundUsers = true
		}
		if s.Name == "orders" && s.Props["storage_kind"] == "table" && s.Props["operation"] == "CREATE" {
			foundOrders = true
		}
	}
	if !foundUsers {
		t.Error("expected storage fact for CREATE TABLE users")
	}
	if !foundOrders {
		t.Error("expected storage fact for CREATE TABLE orders")
	}
}

func TestExtractStorage_SelectQuery(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"internal/repo/user.go": `package repo

import "database/sql"

func GetUsers(db *sql.DB) {
	query := "SELECT id, username FROM users WHERE active = 1"
	_ = query
}
`,
	})

	storage := findFactsByKind(ff, facts.KindStorage)

	found := false
	for _, s := range storage {
		if s.Name == "users" && s.Props["operation"] == "SELECT" {
			found = true
		}
	}
	if !found {
		t.Error("expected storage fact for SELECT FROM users")
	}
}

func TestExtractStorage_InsertUpdateDelete(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"internal/repo/orders.go": `package repo

const insertOrder = "INSERT INTO orders (user_id, total) VALUES (?, ?)"
const updateOrder = "UPDATE orders SET total = ? WHERE id = ?"
const deleteOrder = "DELETE FROM orders WHERE id = ?"
`,
	})

	storage := findFactsByKind(ff, facts.KindStorage)

	ops := make(map[string]bool)
	for _, s := range storage {
		if s.Name == "orders" {
			ops[s.Props["operation"].(string)] = true
		}
	}
	if !ops["INSERT"] {
		t.Error("expected storage fact for INSERT INTO orders")
	}
	if !ops["UPDATE"] {
		t.Error("expected storage fact for UPDATE orders")
	}
	if !ops["DELETE"] {
		t.Error("expected storage fact for DELETE FROM orders")
	}
}

func TestExtractStorage_RawStringLiteral(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"internal/repo/query.go": "package repo\n\nconst q = `SELECT * FROM bookings WHERE user_id = ?`\n",
	})

	storage := findFactsByKind(ff, facts.KindStorage)

	found := false
	for _, s := range storage {
		if s.Name == "bookings" && s.Props["operation"] == "SELECT" {
			found = true
		}
	}
	if !found {
		t.Error("expected storage fact for SELECT FROM bookings in raw string")
	}
}

func TestExtractStorage_Dedup(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"internal/repo/user.go": `package repo

const q1 = "SELECT id FROM users WHERE active = 1"
const q2 = "SELECT name FROM users WHERE id = ?"
`,
	})

	storage := findFactsByKind(ff, facts.KindStorage)

	selectCount := 0
	for _, s := range storage {
		if s.Name == "users" && s.Props["operation"] == "SELECT" {
			selectCount++
		}
	}
	if selectCount != 1 {
		t.Errorf("expected 1 deduplicated SELECT storage fact for users, got %d", selectCount)
	}
}

func TestExtractStorage_NoStorage(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"pkg/util.go": `package util

func Add(a, b int) int {
	return a + b
}
`,
	})

	storage := findFactsByKind(ff, facts.KindStorage)
	if len(storage) != 0 {
		t.Errorf("expected 0 storage facts for non-storage file, got %d", len(storage))
	}
}

func TestExtractStorage_AlterTable(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"pkg/db/migrations.go": `package db

const migration = "ALTER TABLE users ADD COLUMN phone VARCHAR(20)"
`,
	})

	storage := findFactsByKind(ff, facts.KindStorage)

	found := false
	for _, s := range storage {
		if s.Name == "users" && s.Props["operation"] == "ALTER" && s.Props["storage_kind"] == "table" {
			found = true
		}
	}
	if !found {
		t.Error("expected storage fact for ALTER TABLE users")
	}
}
