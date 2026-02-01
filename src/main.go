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
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/joho/godotenv"
	_ "github.com/mattn/go-sqlite3"
)

var db *sql.DB = nil

var token string
var channel_id string
var msgCache map[string]Message
var msgCacheMu sync.RWMutex
var timeLocation *time.Location

func main() {
	err := godotenv.Load()
	if err != nil {
		//log.Fatal("Error loading .env file")
		log.Println("Warning: No .env file found!")
	}

	token = os.Getenv("TOKEN")
	channel_id = os.Getenv("CHANNEL")
	timeLocation = loadTimeLocation(os.Getenv("TIMEZONE"))

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

	ready := make(chan struct{})
	var readyOnce sync.Once
	discord.AddHandler(func(s *discordgo.Session, r *discordgo.Ready) {
		readyOnce.Do(func() {
			close(ready)
		})
	})

	discord.Identify.Intents = discordgo.IntentsGuildMessages | discordgo.IntentsGuilds

	err = discord.Open()
	if err != nil {
		fmt.Println("error opening connection,", err)
		return
	}

	select {
	case <-ready:
	case <-time.After(10 * time.Second):
		log.Println("Warning: Timed out waiting for READY event.")
	}

	lastRun, ok, err := getLastRun()
	if err != nil {
		logError(err)
	} else if ok {
		log.Printf("Refreshing messages since %s", lastRun.UTC().Format(time.RFC3339))
		if err := refreshMessagesSince(discord, lastRun); err != nil {
			logError(err)
		}
	}

	fmt.Println("Bot is now running.  Press CTRL-C to exit.")
	sc := make(chan os.Signal, 1)
	signal.Notify(sc, syscall.SIGINT, syscall.SIGTERM, os.Interrupt)
	<-sc

	now := time.Now().UTC()
	log.Printf("Recording last run time: %s", now.Format(time.RFC3339))
	if err := setLastRun(now); err != nil {
		logError(err)
	}
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

	sqlCreateMetadataTable := `CREATE TABLE IF NOT EXISTS metadata (key TEXT PRIMARY KEY, value TEXT);`
	_, err = db.Exec(sqlCreateMetadataTable)
	if err != nil {
		logWarning(err)
	}

	return db, nil
}

func uploadAllMessages(s *discordgo.Session, m *discordgo.MessageCreate) {
	uploadMessagesSince(s, m, time.Time{})
}

func uploadMessagesSince(s *discordgo.Session, m *discordgo.MessageCreate, since time.Time) {
	channels, err := s.GuildChannels(m.GuildID)
	if err != nil {
		s.ChannelMessageSend(m.ChannelID, fmt.Sprintf("Failed to init db! Err: %q", err))
	}

	for _, channel := range channels {
		if channel.ID == channel_id || channel.Type != discordgo.ChannelTypeGuildText {
			continue
		}

		dyn_message, err := s.ChannelMessageSend(m.ChannelID, fmt.Sprintf("Loading %s!", channel.Name))
		if err != nil {
			logError(err)
			return
		}

		count, err := backfillChannelMessages(s, channel.ID, since)
		if err != nil {
			logError(err)
		}
		s.ChannelMessageEdit(m.ChannelID, dyn_message.ID, fmt.Sprintf("Loading %s (%d)!", channel.Name, count))
	}
}

func messageCreateHandler(s *discordgo.Session, m *discordgo.MessageCreate) {
	if m.Author.ID == s.State.User.ID {
		return
	}

	if m.ChannelID == channel_id {
		if strings.HasPrefix(m.Content, "/init_db") {
			arg := strings.TrimSpace(strings.TrimPrefix(m.Content, "/init_db"))
			if arg == "" {
				s.ChannelMessageSend(m.ChannelID, "Starting DB Init!")
				uploadAllMessages(s, m)
				s.ChannelMessageSend(m.ChannelID, "DB Init Done!")
			} else {
				since, err := parseDateArg(arg)
				if err != nil {
					s.ChannelMessageSend(m.ChannelID, fmt.Sprintf("Invalid date format: %s", err))
				} else {
					s.ChannelMessageSend(m.ChannelID, fmt.Sprintf("Starting DB Init since %s!", since.UTC().Format(time.RFC3339)))
					uploadMessagesSince(s, m, since)
					s.ChannelMessageSend(m.ChannelID, "DB Init Done!")
				}
			}
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

func refreshMessagesSince(s *discordgo.Session, since time.Time) error {
	if since.IsZero() {
		return nil
	}

	if len(s.State.Guilds) == 0 {
		return fmt.Errorf("no guilds available in state (IntentsGuilds required)")
	}

	for _, guild := range s.State.Guilds {
		channels, err := s.GuildChannels(guild.ID)
		if err != nil {
			logError(err)
			continue
		}
		for _, channel := range channels {
			if channel.ID == channel_id || channel.Type != discordgo.ChannelTypeGuildText {
				continue
			}
			count, err := backfillChannelMessages(s, channel.ID, since)
			if err != nil {
				logError(err)
				continue
			}
			log.Printf("Refreshed %d messages from #%s", count, channel.Name)
		}
	}

	return nil
}

func backfillChannelMessages(s *discordgo.Session, channelID string, since time.Time) (int, error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}

	lastMessageID := ""
	total := 0
	done := false

	for {
		messages, err := s.ChannelMessages(channelID, 100, lastMessageID, "", "")
		if err != nil {
			tx.Rollback()
			return total, err
		}
		if len(messages) == 0 {
			break
		}

		for _, message := range messages {
			if !since.IsZero() && message.Timestamp.Before(since) {
				done = true
				break
			}

			if err := upsertMessage(tx, fromMessage(message)); err != nil {
				tx.Rollback()
				return total, err
			}
			total++
		}

		if done || len(messages) < 100 {
			break
		}
		lastMessageID = messages[len(messages)-1].ID
	}

	if err := tx.Commit(); err != nil {
		return total, err
	}

	return total, nil
}

func parseDateArg(input string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339Nano, input); err == nil {
		return t, nil
	}
	if t, err := time.Parse(time.RFC3339, input); err == nil {
		return t, nil
	}

	layouts := []string{
		"2006-01-02 15:04:05",
		"2006-01-02 15:04",
		"2006-01-02",
	}

	loc := timeLocation
	if loc == nil {
		loc = time.UTC
	}

	for _, layout := range layouts {
		if t, err := time.ParseInLocation(layout, input, loc); err == nil {
			return t, nil
		}
	}

	return time.Time{}, fmt.Errorf("use RFC3339 or YYYY-MM-DD (got %q)", input)
}

func loadTimeLocation(tz string) *time.Location {
	if tz == "" {
		return time.UTC
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		log.Printf("Invalid TIMEZONE %q, defaulting to UTC: %v", tz, err)
		return time.UTC
	}
	log.Printf("Using TIMEZONE %s", loc.String())
	return loc
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
