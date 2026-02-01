package main

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/bwmarrin/discordgo"
	"github.com/joho/godotenv"
	_ "github.com/mattn/go-sqlite3"
)

var db *sql.DB = nil

var token string
var channel_id string
var msgCache map[string]Message
var msgCacheMu sync.RWMutex

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

	msgCache = make(map[string]Message)

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

	cacheMessage(m.ID, fromMessageCreate(m))

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

	if m.ID == "" || m.ChannelID == "" {
		return
	}

	updated, err := s.ChannelMessage(m.ChannelID, m.ID)
	if err != nil {
		logError(err)
		return
	}
	if updated.Author != nil && updated.Author.ID == s.State.User.ID {
		return
	}

	after := fromMessage(updated)

	before, ok := getCachedMessage(updated.ID)
	includeEmbeds := ok

	if !ok && m.BeforeUpdate != nil {
		before = fromMessage(m.BeforeUpdate)
		ok = true
		includeEmbeds = true
	}

	if !ok {
		id, err := strconv.Atoi(updated.ID)
		if err == nil {
			message, err := selectMessage(id)
			if err == nil {
				before = message
				ok = true
				includeEmbeds = false
			}
		}
	}

	if ok {
		diff := diffMessages(before, after, includeEmbeds)
		if diff.hasChanges() {
			discordMsg := formatEditLog(updated, diff)
			s.ChannelMessageSendComplex(channel_id, &discordgo.MessageSend{
				Content:         discordMsg,
				AllowedMentions: &discordgo.MessageAllowedMentions{},
			})
		}
	}

	cacheMessage(updated.ID, after)

	tx, err := db.Begin()
	if err != nil {
		logError(err)
		return
	}

	if err := upsertMessage(tx, after); err != nil {
		logError(err)
		tx.Rollback()
		return
	}

	if err := tx.Commit(); err != nil {
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

	deleteCachedMessage(m.ID)
}

type MessageDiff struct {
	contentChanged     bool
	beforeContent      string
	afterContent       string
	attachmentsAdded   []string
	attachmentsRemoved []string
	embedsAdded        []string
	embedsRemoved      []string
}

func (d MessageDiff) hasChanges() bool {
	return d.contentChanged ||
		len(d.attachmentsAdded) > 0 ||
		len(d.attachmentsRemoved) > 0 ||
		len(d.embedsAdded) > 0 ||
		len(d.embedsRemoved) > 0
}

type mediaItem struct {
	key   string
	label string
}

func diffMessages(before, after Message, includeEmbeds bool) MessageDiff {
	diff := MessageDiff{
		beforeContent: before.content,
		afterContent:  after.content,
	}

	diff.contentChanged = before.content != after.content

	diff.attachmentsAdded, diff.attachmentsRemoved = diffMedia(
		attachmentItems(before.attachments),
		attachmentItems(after.attachments),
	)

	if includeEmbeds {
		diff.embedsAdded, diff.embedsRemoved = diffMedia(
			embedItems(before.embeds),
			embedItems(after.embeds),
		)
	}

	return diff
}

func diffMedia(before, after []mediaItem) ([]string, []string) {
	beforeMap := make(map[string]string, len(before))
	for _, item := range before {
		beforeMap[item.key] = item.label
	}
	afterMap := make(map[string]string, len(after))
	for _, item := range after {
		afterMap[item.key] = item.label
	}

	added := []string{}
	removed := []string{}
	for key, label := range afterMap {
		if _, ok := beforeMap[key]; !ok {
			added = append(added, label)
		}
	}
	for key, label := range beforeMap {
		if _, ok := afterMap[key]; !ok {
			removed = append(removed, label)
		}
	}

	sort.Strings(added)
	sort.Strings(removed)

	return added, removed
}

func attachmentItems(attachments []Attachment) []mediaItem {
	items := make([]mediaItem, 0, len(attachments))
	for _, attachment := range attachments {
		items = append(items, mediaItem{
			key:   attachmentKey(attachment),
			label: attachmentLabel(attachment),
		})
	}
	return items
}

func embedItems(embeds []Embed) []mediaItem {
	items := make([]mediaItem, 0, len(embeds))
	for _, embed := range embeds {
		items = append(items, mediaItem{
			key:   embedKey(embed),
			label: embedLabel(embed),
		})
	}
	return items
}

func attachmentKey(a Attachment) string {
	if a.url != "" {
		return "url:" + a.url
	}
	if a.filename != "" {
		return "file:" + a.filename
	}
	if a.id != 0 {
		return fmt.Sprintf("id:%d", a.id)
	}
	return "attachment"
}

func attachmentLabel(a Attachment) string {
	if a.filename != "" && a.url != "" {
		return fmt.Sprintf("%s (%s)", a.filename, a.url)
	}
	if a.url != "" {
		return a.url
	}
	if a.filename != "" {
		return a.filename
	}
	if a.id != 0 {
		return fmt.Sprintf("attachment:%d", a.id)
	}
	return "attachment"
}

func embedKey(e Embed) string {
	return fmt.Sprintf("%s|%s|%s|%s", e.embedType, e.url, e.title, e.provider)
}

func embedLabel(e Embed) string {
	if e.title != "" && e.url != "" {
		return fmt.Sprintf("%s (%s)", e.title, e.url)
	}
	if e.url != "" {
		return e.url
	}
	if e.title != "" {
		return e.title
	}
	if e.provider != "" {
		return e.provider
	}
	if e.embedType != "" {
		return e.embedType
	}
	return "embed"
}

func formatEditLog(updated *discordgo.Message, diff MessageDiff) string {
	var b strings.Builder

	b.WriteString("Message altered:")
	b.WriteString(fmt.Sprintf("\n\tID=%s", updated.ID))
	b.WriteString(fmt.Sprintf("\n\tChannelID=%s", updated.ChannelID))

	if updated.Author != nil {
		authorName := updated.Author.GlobalName
		if authorName == "" {
			authorName = updated.Author.Username
		}
		b.WriteString(fmt.Sprintf("\n\tUser: %s <@%s>", authorName, updated.Author.ID))
	}

	if diff.contentChanged {
		b.WriteString("\n\tBefore:")
		b.WriteString(escapeMessage(diff.beforeContent))
		b.WriteString("\n\tAfter:")
		b.WriteString(escapeMessage(diff.afterContent))
	}

	if len(diff.attachmentsAdded) > 0 {
		b.WriteString("\n\tAttachments added:")
		appendList(&b, diff.attachmentsAdded)
	}
	if len(diff.attachmentsRemoved) > 0 {
		b.WriteString("\n\tAttachments removed:")
		appendList(&b, diff.attachmentsRemoved)
	}
	if len(diff.embedsAdded) > 0 {
		b.WriteString("\n\tEmbeds added:")
		appendList(&b, diff.embedsAdded)
	}
	if len(diff.embedsRemoved) > 0 {
		b.WriteString("\n\tEmbeds removed:")
		appendList(&b, diff.embedsRemoved)
	}

	return b.String()
}

func appendList(b *strings.Builder, items []string) {
	for _, item := range items {
		b.WriteString("\n\t- ")
		b.WriteString(item)
	}
}

func cacheMessage(messageID string, message Message) {
	msgCacheMu.Lock()
	msgCache[messageID] = message
	msgCacheMu.Unlock()
}

func getCachedMessage(messageID string) (Message, bool) {
	msgCacheMu.RLock()
	message, ok := msgCache[messageID]
	msgCacheMu.RUnlock()
	return message, ok
}

func deleteCachedMessage(messageID string) {
	msgCacheMu.Lock()
	delete(msgCache, messageID)
	msgCacheMu.Unlock()
}
