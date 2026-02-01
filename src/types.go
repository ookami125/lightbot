package main

import (
	"strconv"

	"github.com/bwmarrin/discordgo"
)

type Author struct {
	id         int
	globalName string
}

type Attachment struct {
	id       int
	filename string
	url      string
}

type Embed struct {
	url       string
	title     string
	embedType string
	provider  string
}

type Message struct {
	id          int
	author      Author
	content     string
	attachments []Attachment
	embeds      []Embed
}

func toIntOr0(str string) int {
	id, err := strconv.Atoi(str)
	if err != nil {
		id = 0
	}
	return id
}

func toAuthor(u *discordgo.User) Author {
	id := toIntOr0(u.ID)
	return Author{
		id:         id,
		globalName: u.GlobalName,
	}
}

func toAttachment(a *discordgo.MessageAttachment) Attachment {
	id := toIntOr0(a.ID)
	return Attachment{
		id:       id,
		filename: a.Filename,
		url:      a.URL,
	}
}

func toAttachments(a []*discordgo.MessageAttachment) []Attachment {
	attachments := []Attachment{}
	for _, attachment := range a {
		attachments = append(attachments, toAttachment(attachment))
	}
	return attachments
}

func toEmbed(e *discordgo.MessageEmbed) Embed {
	provider := ""
	if e.Provider != nil {
		provider = e.Provider.Name
	}
	return Embed{
		url:       e.URL,
		title:     e.Title,
		embedType: string(e.Type),
		provider:  provider,
	}
}

func toEmbeds(e []*discordgo.MessageEmbed) []Embed {
	embeds := []Embed{}
	for _, embed := range e {
		if embed == nil {
			continue
		}
		embeds = append(embeds, toEmbed(embed))
	}
	return embeds
}

func fromMessageCreate(m *discordgo.MessageCreate) Message {
	id := toIntOr0(m.ID)
	return Message{
		id:          id,
		author:      toAuthor(m.Author),
		content:     m.Content,
		attachments: toAttachments(m.Attachments),
		embeds:      toEmbeds(m.Embeds),
	}
}

func fromMessage(m *discordgo.Message) Message {
	id := toIntOr0(m.ID)
	return Message{
		id:          id,
		author:      toAuthor(m.Author),
		content:     m.Content,
		attachments: toAttachments(m.Attachments),
		embeds:      toEmbeds(m.Embeds),
	}
}

func fromMessageDelete(m *discordgo.MessageDelete) Message {
	id := toIntOr0(m.ID)
	return Message{
		id:          id,
		author:      toAuthor(m.Author),
		content:     m.Content,
		attachments: toAttachments(m.Attachments),
		embeds:      toEmbeds(m.Embeds),
	}
}
