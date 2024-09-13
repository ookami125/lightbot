package main

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/bwmarrin/discordgo"
	"github.com/joho/godotenv"
	_ "github.com/mattn/go-sqlite3"
)

var db *sql.DB = nil

var token string
var channel_id string

func main() {
	err := godotenv.Load()
	if err != nil {
		//log.Fatal("Error loading .env file")
		log.Println("Warning: No .env file found!")
	}

	token = os.Getenv("TOKEN")
	channel_id = os.Getenv("CHANNEL")

	discord, err := discordgo.New(fmt.Sprintf("Bot %s", token))
	if err != nil {
		fmt.Println("error creating Discord session,", err)
		return
	}
	defer discord.Close()

	log.Println("Initing DB!")
	db, err = initDB()
	if err != nil {
		log.Fatal("Error initializing db")
	}
	defer db.Close()

	discord.AddHandler(messageCreateHandler)
	discord.AddHandler(messageDeleteHandler)

	discord.Identify.Intents = discordgo.IntentsGuildMessages

	err = discord.Open()
	if err != nil {
		fmt.Println("error opening connection,", err)
		return
	}

	fmt.Println("Bot is now running.  Press CTRL-C to exit.")
	sc := make(chan os.Signal, 1)
	signal.Notify(sc, syscall.SIGINT, syscall.SIGTERM, os.Interrupt)
	<-sc
}

func initDB() (*sql.DB, error) {
	db, err := sql.Open("sqlite3", "messages.db")
	if err != nil {
		log.Fatal(err)
	}

	sqlCreateMessagesTable := `CREATE TABLE messages (message_id INTEGER NOT NULL PRIMARY KEY, user_id INTEGER NOT NULL, username TEXT, content TEXT);`
	_, err = db.Exec(sqlCreateMessagesTable)
	if err != nil {
		log.Printf("%q: %s\n", err, sqlCreateMessagesTable)
		//return nil, err
	}

	sqlCreateAttachmentsTable := `CREATE TABLE attachments (attachment_id INTEGER NOT NULL PRIMARY KEY, message_id INTEGER NOT NULL, filename STRING, url STRING);`
	_, err = db.Exec(sqlCreateAttachmentsTable)
	if err != nil {
		log.Printf("%q: %s\n", err, sqlCreateAttachmentsTable)
		//return nil, err
	}

	return db, nil
}

func uploadAllMessages(s *discordgo.Session, m *discordgo.MessageCreate) {
	channels, err := s.GuildChannels(m.GuildID)
	if err != nil {
		s.ChannelMessageSend(m.ChannelID, fmt.Sprintf("Failed to init db! Err: %q", err))
	}

	for _, channel := range channels {
		log.Println(channel.Name)

		if channel.ID == channel_id {
			return
		}

		if channel.Type == discordgo.ChannelTypeGuildVoice {
			continue
		}
		if channel.Type == discordgo.ChannelTypeGuildCategory {
			continue
		}

		dyn_message, err := s.ChannelMessageSend(m.ChannelID, fmt.Sprintf("Loading %s!", channel.Name))
		if err != nil {
			log.Printf("ERROR: %q\n", err)
			return
		}

		tx, err := db.Begin()
		if err != nil {
			log.Printf("ERROR: %q\n", err)
			return
		}

		last_message_id := ""
		count := 0
		for {
			s.ChannelMessageEdit(m.ChannelID, dyn_message.ID, fmt.Sprintf("Loading %s (%d+)!", channel.Name, count))
			messages, err := s.ChannelMessages(channel.ID, 100, last_message_id, "", "")
			if err != nil {
				s.ChannelMessageSend(m.ChannelID, fmt.Sprintf("Failed to init db! Err: %q", err))
			}

			for _, message := range messages {
				storeMessage(tx, message)
				last_message_id = message.ID
			}

			count += len(messages)
			if len(messages) < 100 {
				break
			}
		}
		s.ChannelMessageEdit(m.ChannelID, dyn_message.ID, fmt.Sprintf("Loading %s (%d)!", channel.Name, count))

		err = tx.Commit()
		if err != nil {
			log.Fatal(err)
		}
	}
}

func storeMessage(tx *sql.Tx, m *discordgo.Message) {
	sqlAddMessage := `INSERT OR IGNORE INTO messages(message_id, user_id, username, content) VALUES (?, ?, ?, ?)`
	_, err := tx.Exec(sqlAddMessage, m.ID, m.Author.ID, m.Author.GlobalName, m.Content)
	if err != nil {
		log.Printf("%q\n", err)
		return
	}

	sqlAddAttachment := `INSERT OR IGNORE INTO attachments(attachment_id, message_id, filename, url) VALUES (?, ?, ?, ?)`
	for _, attachment := range m.Attachments {
		_, err := tx.Exec(sqlAddAttachment, attachment.ID, m.ID, attachment.Filename, attachment.URL)
		if err != nil {
			log.Printf("%q\n", err)
			return
		}
	}
}

func messageCreateHandler(s *discordgo.Session, m *discordgo.MessageCreate) {
	if m.Author.ID == s.State.User.ID {
		return
	}

	if m.ChannelID == channel_id {
		if strings.HasPrefix(m.Content, "/init_db") {
			s.ChannelMessageSend(m.ChannelID, "Starting DB Init!")
			uploadAllMessages(s, m)
			s.ChannelMessageSend(m.ChannelID, "DB Init Done!")
		}
		return
	}

	tx, err := db.Begin()
	if err != nil {
		log.Printf("ERROR: %q\n", err)
		return
	}

	storeMessage(tx, m.Message)

	err = tx.Commit()
	if err != nil {
		log.Printf("ERROR: %q\n", err)
		return
	}
}

func messageDeleteHandler(s *discordgo.Session, m *discordgo.MessageDelete) {
	if m.ChannelID == channel_id {
		return
	}

	sqlGetMessage := `SELECT user_id, username, content FROM messages WHERE message_id=?`
	rows, err := db.Query(sqlGetMessage, m.ID)
	if err != nil {
		log.Printf("%q: %s\n", err, sqlGetMessage)
		return
	}
	defer rows.Close()

	flag := true
	for rows.Next() {
		var user_id int
		var username string
		var content string
		err = rows.Scan(&user_id, &username, &content)
		if err != nil {
			log.Printf("ERROR: %q\n", err)
			break
		}

		content = strings.ReplaceAll(content, "`", "'")

		if content == "" {
			content = "<Empty>"
		} else {
			content = "```" + content + "```"
		}

		msg := fmt.Sprintf("Message deleted:\n\tID=%s\n\tChannelID=%s\n\tUser: %s <@%d>\n\tContent:%s", m.ID, m.ChannelID, username, user_id, content)

		sqlGetMessage := `SELECT attachment_id, filename, url FROM attachments WHERE message_id=?`
		attach_rows, err := db.Query(sqlGetMessage, m.ID)
		if err != nil {
			log.Printf("%q: %s\n", err, sqlGetMessage)
		} else {
			defer attach_rows.Close()

			attach_first := true
			for attach_rows.Next() {
				var attachment_id int
				var filename string
				var url string
				err = attach_rows.Scan(&attachment_id, &filename, &url)
				if err != nil {
					log.Println(err)
					continue
				}

				if attach_first {
					attach_first = false
					msg = fmt.Sprintf("%s\n\tAttachments:", msg)
				}

				msg = fmt.Sprintf("%s\n\tID: %d [%s](%s):", msg, attachment_id, filename, url)
			}
		}

		var complex_msg = &discordgo.MessageSend{
			Content:         msg,
			AllowedMentions: &discordgo.MessageAllowedMentions{},
		}

		s.ChannelMessageSendComplex(channel_id, complex_msg)
		flag = false
	}

	if flag {
		s.ChannelMessageSend(channel_id, fmt.Sprintf("Message deleted but no data stored!\n\tID=%s\n\tChannelID=%s", m.ID, m.ChannelID))
	}
}
