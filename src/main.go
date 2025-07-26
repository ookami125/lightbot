package main

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
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
	discord.AddHandler(messageEditHandler)
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
		logWarning(err)
	}

	sqlCreateAttachmentsTable := `CREATE TABLE attachments (attachment_id INTEGER NOT NULL PRIMARY KEY, message_id INTEGER NOT NULL, filename STRING, url STRING);`
	_, err = db.Exec(sqlCreateAttachmentsTable)
	if err != nil {
		logWarning(err)
	}

	return db, nil
}

func uploadAllMessages(s *discordgo.Session, m *discordgo.MessageCreate) {
	channels, err := s.GuildChannels(m.GuildID)
	if err != nil {
		s.ChannelMessageSend(m.ChannelID, fmt.Sprintf("Failed to init db! Err: %q", err))
	}

	for _, channel := range channels {
		if channel.ID == channel_id {
			continue
		}

		dyn_message, err := s.ChannelMessageSend(m.ChannelID, fmt.Sprintf("Loading %s!", channel.Name))
		if err != nil {
			logError(err)
			return
		}

		tx, err := db.Begin()
		if err != nil {
			logError(err)
			return
		}

		last_message_id := ""
		count := 0
		for {
			s.ChannelMessageEdit(m.ChannelID, dyn_message.ID, fmt.Sprintf("Loading %s (%d+)!", channel.Name, count))
			messages, err := s.ChannelMessages(channel.ID, 100, last_message_id, "", "")
			if err != nil {
				logError(err)
				s.ChannelMessageSend(m.ChannelID, fmt.Sprintf("Failed to init db! Err: %q", err))
			}

			for _, message := range messages {
				upsertMessage(tx, fromMessage(message))
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
		logError(err)
		return
	}

	insertMessage(fromMessageCreate(m))

	err = tx.Commit()
	if err != nil {
		logError(err)
		return
	}
}

func escapeMessage(content string) string {
	if content == "" {
		return "<Empty>"
	}

	content = strings.ReplaceAll(content, "`", "'")
	content = "```" + content + "```"

	return content
}

func messageEditHandler(s *discordgo.Session, m *discordgo.MessageUpdate) {
	if m.ChannelID == channel_id {
		return
	}

	id, err := strconv.Atoi(m.ID)
	if err != nil {
		logError(err)
		return
	}
	message, err := selectMessage(id)
	if err != nil {
		logError(err)
		return
	}

	oldContent := escapeMessage(message.content)
	newContent := escapeMessage(m.Content)

	discord_msg := fmt.Sprintf("Message altered:\n\tID=%s\n\tChannelID=%s\n\tUser: %s <@%d>\n\tBefore:%s\n\tAfter:%s", m.ID, m.ChannelID, message.author.globalName, message.author.id, oldContent, newContent)

	s.ChannelMessageSendComplex(channel_id, &discordgo.MessageSend{
		Content:         discord_msg,
		AllowedMentions: &discordgo.MessageAllowedMentions{},
	})

	tx, err := db.Begin()
	if err != nil {
		logError(err)
		return
	}

	message.content = m.Content
	message.author.globalName = m.Author.GlobalName

	upsertMessage(tx, message)

	err = tx.Commit()
	if err != nil {
		logError(err)
		return
	}
}

func messageDeleteHandler(s *discordgo.Session, m *discordgo.MessageDelete) {
	if m.ChannelID == channel_id {
		return
	}

	id, err := strconv.Atoi(m.ID)
	if err != nil {
		logError(err)
		return
	}
	message, err := selectMessage(id)
	if err != nil {
		logError(err)
		return
	}

	content := escapeMessage(message.content)

	discord_msg := fmt.Sprintf("Message deleted:\n\tID=%s\n\tChannelID=%s\n\tUser: %s <@%d>\n\tContent:%s", m.ID, m.ChannelID, message.author.globalName, message.author.id, content)

	if len(message.attachments) > 0 {
		discord_msg = fmt.Sprintf("%s\n\tAttachments:", discord_msg)
	}

	for _, attachment := range message.attachments {
		discord_msg = fmt.Sprintf("%s\n\tID: %d [%s](%s):", discord_msg, attachment.id, attachment.filename, attachment.url)

	}

	s.ChannelMessageSendComplex(channel_id, &discordgo.MessageSend{
		Content:         discord_msg,
		AllowedMentions: &discordgo.MessageAllowedMentions{},
	})
}
