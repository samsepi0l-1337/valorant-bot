package valorantbot

import (
	"context"

	"github.com/bwmarrin/discordgo"
)

// discordPoster adapts discordgo to scheduler ChannelPoster / DMPoster.
type discordPoster struct {
	session *discordgo.Session
}

func (p *discordPoster) PostChannel(_ context.Context, channelID, content string, embeds []*discordgo.MessageEmbed) error {
	_, err := p.session.ChannelMessageSendComplex(channelID, &discordgo.MessageSend{
		Content: content,
		Embeds:  embeds,
	})
	return err
}

func (p *discordPoster) SendDM(_ context.Context, discordUserID, content string, embeds []*discordgo.MessageEmbed) error {
	ch, err := p.session.UserChannelCreate(discordUserID)
	if err != nil {
		return err
	}
	_, err = p.session.ChannelMessageSendComplex(ch.ID, &discordgo.MessageSend{
		Content: content,
		Embeds:  embeds,
	})
	return err
}
