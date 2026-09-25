package messaging

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

type Mutation struct {
	MessageID string `json:"message_id"`
	ChannelID string `json:"channel_id"`
}

// Handle runs one explicitly bound messaging behavior in the caller's transaction.
func Handle(ctx context.Context, tx *sql.Tx, worldID, runID, callID, behavior string, args map[string]any) (int, any, any, error) {
	switch behavior {
	case "messaging.listChannels":
		id, ok := args["id"].(string)
		if !ok || id == "" {
			return 400, map[string]string{"error": "invalid workspace id"}, nil, nil
		}
		var workspace string
		err := tx.QueryRowContext(ctx, "SELECT id FROM message_workspaces WHERE world_id=? AND id=?", worldID, id).Scan(&workspace)
		if errors.Is(err, sql.ErrNoRows) {
			return 404, map[string]string{"error": "workspace not found"}, nil, nil
		}
		if err != nil {
			return 0, nil, nil, err
		}
		rows, err := tx.QueryContext(ctx, "SELECT id,name FROM message_channels WHERE world_id=? AND workspace_id=? ORDER BY id", worldID, id)
		if err != nil {
			return 0, nil, nil, err
		}
		defer rows.Close()
		out := []map[string]any{}
		for rows.Next() {
			var channelID, name string
			if err = rows.Scan(&channelID, &name); err != nil {
				return 0, nil, nil, err
			}
			out = append(out, map[string]any{"id": channelID, "workspace_id": workspace, "name": name})
		}
		return 200, out, nil, rows.Err()
	case "messaging.readChannel":
		id, ok := args["id"].(string)
		if !ok || id == "" {
			return 400, map[string]string{"error": "invalid channel id"}, nil, nil
		}
		var workspace, name string
		err := tx.QueryRowContext(ctx, "SELECT workspace_id,name FROM message_channels WHERE world_id=? AND id=?", worldID, id).Scan(&workspace, &name)
		if errors.Is(err, sql.ErrNoRows) {
			return 404, map[string]string{"error": "channel not found"}, nil, nil
		}
		if err != nil {
			return 0, nil, nil, err
		}
		rows, err := tx.QueryContext(ctx, "SELECT id,body,created_at FROM message_messages WHERE world_id=? AND channel_id=? ORDER BY created_at,id", worldID, id)
		if err != nil {
			return 0, nil, nil, err
		}
		defer rows.Close()
		messages := []map[string]any{}
		for rows.Next() {
			var messageID, body, at string
			if err = rows.Scan(&messageID, &body, &at); err != nil {
				return 0, nil, nil, err
			}
			messages = append(messages, map[string]any{"id": messageID, "channel_id": id, "body": body, "created_at": at})
		}
		if err = rows.Err(); err != nil {
			return 0, nil, nil, err
		}
		return 200, map[string]any{"id": id, "workspace_id": workspace, "name": name, "messages": messages}, nil, nil
	case "messaging.postMessage":
		channelID, channelOK := args["channel_id"].(string)
		body, bodyOK := args["body"].(string)
		if !channelOK || strings.TrimSpace(channelID) == "" || !bodyOK || strings.TrimSpace(body) == "" {
			return 400, map[string]string{"error": "invalid message arguments"}, nil, nil
		}
		var existing string
		err := tx.QueryRowContext(ctx, "SELECT id FROM message_channels WHERE world_id=? AND id=?", worldID, channelID).Scan(&existing)
		if errors.Is(err, sql.ErrNoRows) {
			return 404, map[string]string{"error": "channel not found"}, nil, nil
		}
		if err != nil {
			return 0, nil, nil, err
		}
		var base string
		if err = tx.QueryRowContext(ctx, "SELECT base_at FROM worlds WHERE id=?", worldID).Scan(&base); err != nil {
			return 0, nil, nil, err
		}
		baseTime, err := time.Parse(time.RFC3339, base)
		if err != nil {
			return 0, nil, nil, err
		}
		sum := sha256.Sum256([]byte(runID + ":" + callID))
		messageID := "MSG-" + hex.EncodeToString(sum[:6])
		at := baseTime.Add(24 * time.Hour).Format(time.RFC3339)
		if _, err = tx.ExecContext(ctx, "INSERT INTO message_messages(world_id,id,channel_id,body,created_at) VALUES(?,?,?,?,?)", worldID, messageID, channelID, body, at); err != nil {
			return 0, nil, nil, err
		}
		return 201, map[string]any{"id": messageID, "channel_id": channelID, "body": body, "created_at": at}, Mutation{messageID, channelID}, nil
	default:
		return 0, nil, nil, fmt.Errorf("unknown messaging behavior %q", behavior)
	}
}
