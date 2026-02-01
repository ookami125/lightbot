package main

import (
	"database/sql"
	"fmt"
	"log"
	"time"
)

func insertMessage(message Message) error {
	sqlAddMessage := `INSERT INTO messages(message_id, user_id, username, content) VALUES (?, ?, ?, ?)`
	_, err := db.Exec(sqlAddMessage, message.id, message.author.id, message.author.globalName, message.content)
	if err != nil {
		return err
	}

	sqlAddAttachment := `INSERT INTO attachments(attachment_id, message_id, filename, url) VALUES (?, ?, ?, ?)`
	for _, attachment := range message.attachments {
		_, err := db.Exec(sqlAddAttachment, attachment.id, message.id, attachment.filename, attachment.url)
		if err != nil {
			return err
		}
	}

	return nil
}

func upsertMessage(tx *sql.Tx, message Message) error {
	sqlAddMessage := `INSERT INTO messages (message_id, user_id, username, content)
VALUES (?, ?, ?, ?)
ON CONFLICT(message_id)
DO UPDATE SET
    user_id = excluded.user_id,
    username = excluded.username,
    content = excluded.content;`
	_, err := tx.Exec(sqlAddMessage, message.id, message.author.id, message.author.globalName, message.content)
	if err != nil {
		return err
	}

	sqlAddAttachment := `INSERT INTO attachments (attachment_id, message_id, filename, url) VALUES (?, ?, ?, ?)
ON CONFLICT(attachment_id)
DO UPDATE SET
    message_id = excluded.message_id,
    filename = excluded.filename,
    url = excluded.url;`
	for _, attachment := range message.attachments {
		_, err := tx.Exec(sqlAddAttachment, attachment.id, message.id, attachment.filename, attachment.url)
		if err != nil {
			return err
		}
	}

	return nil
}

func loadAttachments(message_id int) ([]Attachment, error) {
	sqlGetMessage := `SELECT attachment_id, filename, url FROM attachments WHERE message_id=?`
	attach_rows, err := db.Query(sqlGetMessage, message_id)
	if err != nil {
		return []Attachment{}, err
	}
	defer attach_rows.Close()

	attachments := []Attachment{}
	for attach_rows.Next() {
		var attachment Attachment
		err = attach_rows.Scan(&attachment.id, &attachment.filename, &attachment.url)
		if err != nil {
			logWarning(err)
			continue
		}

		attachments = append(attachments, attachment)
	}

	return attachments, nil
}

type ErrorNoResults struct {
	id int
}

func (m *ErrorNoResults) Error() string {
	return fmt.Sprintf("No results for %d", m.id)
}

func selectMessage(message_id int) (Message, error) {

	sqlGetMessage := `SELECT message_id, user_id, username, content FROM messages WHERE message_id=?`
	rows, err := db.Query(sqlGetMessage, message_id)
	if err != nil {
		log.Printf("%q: %s\n", err, sqlGetMessage)
		return Message{}, err
	}
	defer rows.Close()

	for rows.Next() {
		var message Message
		err = rows.Scan(&message.id, &message.author.id, &message.author.globalName, &message.content)

		if err != nil {
			logError(err)
			continue
		}

		attachments, err := loadAttachments(message_id)
		if err != nil {
			logWarning(err)
		} else {
			message.attachments = attachments
		}

		return message, nil
	}

	return Message{}, &ErrorNoResults{id: message_id}
}

func setMetadata(key, value string) error {
	_, err := db.Exec(`INSERT INTO metadata (key, value) VALUES (?, ?)
ON CONFLICT(key)
DO UPDATE SET value = excluded.value`, key, value)
	return err
}

func getMetadata(key string) (string, bool, error) {
	var value string
	err := db.QueryRow(`SELECT value FROM metadata WHERE key = ?`, key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return value, true, nil
}

func setLastRun(t time.Time) error {
	return setMetadata("last_run", t.UTC().Format(time.RFC3339Nano))
}

func getLastRun() (time.Time, bool, error) {
	value, ok, err := getMetadata("last_run")
	if err != nil || !ok {
		return time.Time{}, ok, err
	}

	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err == nil {
		return parsed, true, nil
	}
	parsed, err = time.Parse(time.RFC3339, value)
	if err == nil {
		return parsed, true, nil
	}
	return time.Time{}, false, err
}
