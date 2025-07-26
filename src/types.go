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

type Message struct {
	id          int
	author      Author
	content     string
	attachments []Attachment
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

func fromMessageCreate(m *discordgo.MessageCreate) Message {
	id := toIntOr0(m.ID)
	return Message{
		id:          id,
		author:      toAuthor(m.Author),
		content:     m.Content,
		attachments: toAttachments(m.Attachments),
	}
}

func fromMessage(m *discordgo.Message) Message {
	id := toIntOr0(m.ID)
	return Message{
		id:          id,
		author:      toAuthor(m.Author),
		content:     m.Content,
		attachments: toAttachments(m.Attachments),
	}
}

func fromMessageDelete(m *discordgo.MessageDelete) Message {
	id := toIntOr0(m.ID)
	return Message{
		id:          id,
		author:      toAuthor(m.Author),
		content:     m.Content,
		attachments: toAttachments(m.Attachments),
	}
}
