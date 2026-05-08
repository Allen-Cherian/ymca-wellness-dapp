package database

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// CreateAdmin inserts a new admin row.
func CreateAdmin(ctx context.Context, a *Admin) error {
	_, err := Pool.Exec(ctx, `
		INSERT INTO admins (did, node_port, password)
		VALUES ($1, $2, $3)
	`, a.DID, a.NodePort, a.Password)
	if err != nil {
		return fmt.Errorf("CreateAdmin: %w", err)
	}
	return nil
}

// GetAdminByDID fetches one admin by DID.
func GetAdminByDID(ctx context.Context, did string) (*Admin, error) {
	var a Admin
	err := Pool.QueryRow(ctx,
		`SELECT did, node_port, password, created_at FROM admins WHERE did = $1`,
		did,
	).Scan(&a.DID, &a.NodePort, &a.Password, &a.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("GetAdminByDID: %w", err)
	}
	return &a, nil
}

// ListAdmins returns every admin row, ordered by created_at.
func ListAdmins(ctx context.Context) ([]Admin, error) {
	rows, err := Pool.Query(ctx,
		`SELECT did, node_port, password, created_at FROM admins ORDER BY created_at ASC`,
	)
	if err != nil {
		return nil, fmt.Errorf("ListAdmins: %w", err)
	}
	defer rows.Close()

	var out []Admin
	for rows.Next() {
		var a Admin
		if err := rows.Scan(&a.DID, &a.NodePort, &a.Password, &a.CreatedAt); err != nil {
			return nil, fmt.Errorf("ListAdmins scan: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
