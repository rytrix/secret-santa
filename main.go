// Secret Santa Discord bot.
//
// Everything lives under one slash command, /santa, with subcommands.
// Replies that reveal anything secret are ephemeral (only the caller sees
// them), and assignments are also sent by DM.
package main

import (
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"
)

// userErr is an error meant to be shown to the person who ran the command.
type userErr string

func (e userErr) Error() string { return string(e) }

type Bot struct {
	store          *Store
	attachmentsDir string // gift attachments waiting for /santa end
	backupsDir     string // one folder per ended round
}

// dm is a direct message to send after the store lock is released, so
// slow network calls never block other commands.
type dm struct {
	userID string
	text   string
}

func main() {
	token := os.Getenv("DISCORD_TOKEN")
	if token == "" {
		log.Fatal("set DISCORD_TOKEN")
	}
	dataPath := envOr("SANTA_DATA", "santa.json")
	devGuild := os.Getenv("GUILD_ID") // optional: register commands to one server only (instant updates while developing)

	store, err := OpenStore(dataPath)
	if err != nil {
		log.Fatalf("loading %s: %v", dataPath, err)
	}
	bot := &Bot{
		store:          store,
		attachmentsDir: envOr("SANTA_ATTACHMENTS", "santa-attachments"),
		backupsDir:     envOr("SANTA_BACKUPS", "santa-backups"),
	}

	s, err := discordgo.New("Bot " + token)
	if err != nil {
		log.Fatal(err)
	}
	// Slash commands and DMs need no privileged intents.
	s.Identify.Intents = discordgo.IntentsGuilds

	s.AddHandler(func(s *discordgo.Session, r *discordgo.Ready) {
		log.Printf("logged in as %s", r.User.Username)
	})
	s.AddHandler(bot.onInteraction)

	if err := s.Open(); err != nil {
		log.Fatal(err)
	}
	defer s.Close()

	if _, err := s.ApplicationCommandBulkOverwrite(s.State.User.ID, devGuild, commands()); err != nil {
		log.Fatalf("registering commands: %v", err)
	}
	log.Println("running; Ctrl-C to stop")

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// ---------------------------------------------------------------- commands

func commands() []*discordgo.ApplicationCommand {
	no := false
	str := func(name, desc string, required bool) *discordgo.ApplicationCommandOption {
		return &discordgo.ApplicationCommandOption{Type: discordgo.ApplicationCommandOptionString, Name: name, Description: desc, Required: required}
	}
	usr := func(name, desc string) *discordgo.ApplicationCommandOption {
		return &discordgo.ApplicationCommandOption{Type: discordgo.ApplicationCommandOptionUser, Name: name, Description: desc, Required: true}
	}
	att := func(name, desc string) *discordgo.ApplicationCommandOption {
		return &discordgo.ApplicationCommandOption{Type: discordgo.ApplicationCommandOptionAttachment, Name: name, Description: desc}
	}
	// Slash commands take one file per option, so each image is its own option.
	giftOpts := []*discordgo.ApplicationCommandOption{str("text", "A message for your recipient", false)}
	for n := 1; n <= maxAttachments; n++ {
		giftOpts = append(giftOpts, att(fmt.Sprintf("image%d", n), "An image to include (JPEG, PNG, GIF or WebP)"))
	}
	sub := func(name, desc string, opts ...*discordgo.ApplicationCommandOption) *discordgo.ApplicationCommandOption {
		return &discordgo.ApplicationCommandOption{Type: discordgo.ApplicationCommandOptionSubCommand, Name: name, Description: desc, Options: opts}
	}
	return []*discordgo.ApplicationCommand{{
		Name:         "santa",
		Description:  "Secret Santa",
		DMPermission: &no, // must be used in a server, so we know which event
		Options: []*discordgo.ApplicationCommandOption{
			sub("create", "Start a Secret Santa in this server",
				str("date", "When gifts are exchanged, e.g. Dec 20", false)),
			sub("join", "Join this server's Secret Santa",
				str("wishlist", "Ideas for your Santa (you can change it later)", false)),
			sub("leave", "Leave before names are drawn"),
			sub("wishlist", "Set or update your wishlist",
				str("text", "Gift ideas, sizes, things to avoid…", true)),
			sub("exclude", "Don't pair me with this person (toggle)",
				usr("user", "Someone you shouldn't draw, e.g. your partner")),
			sub("list", "Show who has joined"),
			sub("draw", "Organizer: draw names and DM everyone"),
			sub("recipient", "Remind me who I'm buying for"),
			sub("gift", "Set your gift (replaces any earlier one); it's DMed to your recipient when the Secret Santa ends",
				giftOpts...),
			sub("remove", "Organizer: remove someone before the draw",
				usr("user", "Who to remove")),
			sub("end", "Organizer: deliver everyone's gifts by DM and end this Secret Santa"),
		},
	}}
}

func (b *Bot) onInteraction(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if i.Type != discordgo.InteractionApplicationCommand {
		return
	}
	data := i.ApplicationCommandData()
	if data.Name != "santa" || len(data.Options) == 0 || i.Member == nil {
		return
	}
	sub := data.Options[0]
	opts := map[string]*discordgo.ApplicationCommandInteractionDataOption{}
	for _, o := range sub.Options {
		opts[o.Name] = o
	}
	optStr := func(name string) string {
		if o, ok := opts[name]; ok {
			return strings.TrimSpace(o.StringValue())
		}
		return ""
	}
	optUser := func(name string) *discordgo.User {
		if o, ok := opts[name]; ok {
			return o.UserValue(s)
		}
		return nil
	}

	c := &ctx{s: s, i: i, guild: i.GuildID, user: i.Member.User, name: displayName(i.Member)}

	switch sub.Name {
	case "create":
		b.create(c, optStr("date"))
	case "join":
		b.join(c, optStr("wishlist"))
	case "leave":
		b.leave(c)
	case "wishlist":
		b.setWishlist(c, optStr("text"))
	case "exclude":
		b.exclude(c, optUser("user"))
	case "list":
		b.list(c)
	case "draw":
		b.draw(c)
	case "recipient":
		b.myRecipient(c)
	case "gift":
		var atts []*discordgo.MessageAttachment
		for n := 1; n <= maxAttachments; n++ {
			o, ok := opts[fmt.Sprintf("image%d", n)]
			if !ok || data.Resolved == nil {
				continue
			}
			if id, _ := o.Value.(string); data.Resolved.Attachments[id] != nil {
				atts = append(atts, data.Resolved.Attachments[id])
			}
		}
		b.setGift(c, optStr("text"), atts)
	case "remove":
		b.remove(c, optUser("user"))
	case "end":
		b.end(c)
	}
}

// ctx bundles what every handler needs.
type ctx struct {
	s        *discordgo.Session
	i        *discordgo.InteractionCreate
	guild    string
	user     *discordgo.User
	name     string
	deferred bool // deferReply was called; replies edit the placeholder
}

// isAdmin reports whether the caller has Administrator or Manage Server in
// this server. Discord sends the caller's permissions with every command.
func (c *ctx) isAdmin() bool {
	return c.i.Member.Permissions&(discordgo.PermissionAdministrator|discordgo.PermissionManageGuild) != 0
}

func (c *ctx) isOrganizer(e *Event) bool {
	return e.OrganizerID == c.user.ID || c.isAdmin()
}

// private replies only to the caller; public posts in the channel.
func (c *ctx) private(msg string) { c.respond(msg, discordgo.MessageFlagsEphemeral) }
func (c *ctx) public(msg string)  { c.respond(msg, 0) }

// deferReply acknowledges the command now ("Bot is thinking…") for work that
// may take longer than Discord's 3-second reply window. The real reply comes
// later through private/public/fail. Whether it's visible only to the caller
// is decided here and can't change afterwards.
func (c *ctx) deferReply(ephemeral bool) {
	var flags discordgo.MessageFlags
	if ephemeral {
		flags = discordgo.MessageFlagsEphemeral
	}
	err := c.s.InteractionRespond(c.i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Flags: flags},
	})
	if err != nil {
		log.Printf("defer: %v", err)
	}
	c.deferred = true
}

func (c *ctx) respond(msg string, flags discordgo.MessageFlags) {
	if c.deferred {
		_, err := c.s.InteractionResponseEdit(c.i.Interaction, &discordgo.WebhookEdit{
			Content:         &msg,
			AllowedMentions: &discordgo.MessageAllowedMentions{},
		})
		if err != nil {
			log.Printf("edit response: %v", err)
		}
		return
	}
	err := c.s.InteractionRespond(c.i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content:         msg,
			Flags:           flags,
			AllowedMentions: &discordgo.MessageAllowedMentions{}, // show @names without pinging
		},
	})
	if err != nil {
		log.Printf("respond: %v", err)
	}
}

// fail turns an error from a store update into a reply.
func (c *ctx) fail(err error) {
	var ue userErr
	if errors.As(err, &ue) {
		c.private(string(ue))
		return
	}
	log.Printf("error: %v", err)
	c.private("Something went wrong on my end, sorry. Try again in a moment.")
}

func displayName(m *discordgo.Member) string {
	if m.Nick != "" {
		return m.Nick
	}
	if m.User.GlobalName != "" {
		return m.User.GlobalName
	}
	return m.User.Username
}

func mention(id string) string { return "<@" + id + ">" }

func sendDMs(s *discordgo.Session, msgs []dm) (failed []string) {
	for _, m := range msgs {
		ch, err := s.UserChannelCreate(m.userID)
		if err == nil {
			_, err = s.ChannelMessageSend(ch.ID, m.text)
		}
		if err != nil {
			log.Printf("DM to %s failed: %v", m.userID, err)
			failed = append(failed, m.userID)
		}
	}
	return failed
}

// event fetches this server's event or returns a friendly error.
func (b *Bot) event(guild string) (*Event, error) {
	e := b.store.Events[guild]
	if e == nil {
		return nil, userErr("There's no Secret Santa in this server yet. Start one with `/santa create`.")
	}
	return e, nil
}

// ---------------------------------------------------------------- handlers

func (b *Bot) create(c *ctx, date string) {
	// Checked here rather than in Discord's command settings, because Discord
	// can only restrict the whole /santa command, not one subcommand.
	if !c.isAdmin() {
		c.private("Only server admins (Administrator or Manage Server permission) can start a Secret Santa.")
		return
	}
	err := b.store.Update(func() error {
		if b.store.Events[c.guild] != nil {
			return userErr("This server already has a Secret Santa. The organizer can `/santa end` it first.")
		}
		b.store.Events[c.guild] = &Event{
			GuildID:     c.guild,
			ChannelID:   c.i.ChannelID,
			OrganizerID: c.user.ID,
			Date:        date,
			CreatedAt:   time.Now(),
			People:      map[string]*Participant{c.user.ID: {ID: c.user.ID, Name: c.name, Username: c.user.Username}},
			Joined:      []string{c.user.ID},
			Exclusions:  map[string]map[string]bool{},
		}
		return nil
	})
	if err != nil {
		c.fail(err)
		return
	}
	msg := fmt.Sprintf("🎅 **Secret Santa is on!** Organized by %s (already joined).\n", mention(c.user.ID))
	if date != "" {
		msg += fmt.Sprintf("📅 Exchange: %s\n", date)
	}
	msg += "\nJoin with `/santa join`, add gift ideas with `/santa wishlist`. Names get drawn when the organizer runs `/santa draw`."
	c.public(msg)
}

func (b *Bot) join(c *ctx, wishlist string) {
	var count int
	err := b.store.Update(func() error {
		e, err := b.event(c.guild)
		if err != nil {
			return err
		}
		if e.Drawn {
			return userErr("Names have already been drawn, so it's too late to join this one.")
		}
		if p := e.People[c.user.ID]; p != nil {
			return userErr("You're already in! Update your gift ideas with `/santa wishlist`.")
		}
		e.People[c.user.ID] = &Participant{ID: c.user.ID, Name: c.name, Username: c.user.Username, Wishlist: wishlist}
		e.Joined = append(e.Joined, c.user.ID)
		count = len(e.People)
		return nil
	})
	if err != nil {
		c.fail(err)
		return
	}
	c.public(fmt.Sprintf("🎁 %s joined the Secret Santa! (%d people so far)", mention(c.user.ID), count))
}

func (b *Bot) leave(c *ctx) {
	err := b.store.Update(func() error {
		e, err := b.event(c.guild)
		if err != nil {
			return err
		}
		if e.People[c.user.ID] == nil {
			return userErr("You're not in this Secret Santa.")
		}
		if e.Drawn {
			return userErr("Names are already drawn, so leaving now would leave someone without a gift. Ask the organizer if you need out.")
		}
		if e.OrganizerID == c.user.ID {
			return userErr("You're the organizer. Use `/santa end` to cancel the whole thing instead.")
		}
		e.Remove(c.user.ID)
		return nil
	})
	if err != nil {
		c.fail(err)
		return
	}
	c.public(fmt.Sprintf("%s left the Secret Santa.", mention(c.user.ID)))
}

func (b *Bot) setWishlist(c *ctx, text string) {
	var notify []dm
	err := b.store.Update(func() error {
		e, err := b.event(c.guild)
		if err != nil {
			return err
		}
		p := e.People[c.user.ID]
		if p == nil {
			return userErr("Join first with `/santa join`.")
		}
		p.Wishlist = text
		p.Username = c.user.Username // usernames can change; keep the current one
		e.FillRecipientUsernames()
		// After the draw, let this person's Santa know the list changed.
		if santa, ok := e.SantaOf(c.user.ID); ok {
			notify = append(notify, dm{santa, fmt.Sprintf("📝 Your recipient **%s** updated their wishlist:\n> %s", p.Name, quote(text))})
		}
		return nil
	})
	if err != nil {
		c.fail(err)
		return
	}
	c.private("Wishlist saved ✅")
	sendDMs(c.s, notify)
}

func (b *Bot) exclude(c *ctx, other *discordgo.User) {
	var nowExcluded bool
	err := b.store.Update(func() error {
		e, err := b.event(c.guild)
		if err != nil {
			return err
		}
		if e.People[c.user.ID] == nil {
			return userErr("Join first with `/santa join`.")
		}
		if e.Drawn {
			return userErr("Names are already drawn.")
		}
		if other == nil || other.ID == c.user.ID {
			return userErr("You can't exclude yourself (you'll never draw yourself anyway).")
		}
		if e.Exclusions[c.user.ID] == nil {
			e.Exclusions[c.user.ID] = map[string]bool{}
		}
		nowExcluded = !e.Exclusions[c.user.ID][other.ID]
		if nowExcluded {
			e.Exclusions[c.user.ID][other.ID] = true
		} else {
			delete(e.Exclusions[c.user.ID], other.ID)
		}
		return nil
	})
	if err != nil {
		c.fail(err)
		return
	}
	if nowExcluded {
		c.private(fmt.Sprintf("Got it: you and %s won't be paired with each other (either direction). Run this again to undo.", mention(other.ID)))
	} else {
		c.private(fmt.Sprintf("Exclusion removed: you and %s can be paired again.", mention(other.ID)))
	}
}

func (b *Bot) list(c *ctx) {
	var msg string
	err := b.store.View(func() error {
		e, err := b.event(c.guild)
		if err != nil {
			return err
		}
		var sb strings.Builder
		status := "open for joining"
		if e.Drawn {
			status = "names drawn"
		}
		fmt.Fprintf(&sb, "🎅 **Secret Santa** (%s), organized by %s\n", status, mention(e.OrganizerID))
		if e.Date != "" {
			fmt.Fprintf(&sb, "📅 Exchange: %s\n", e.Date)
		}
		fmt.Fprintf(&sb, "\n**%d people:**\n", len(e.Joined))
		for _, id := range e.Joined {
			mark := ""
			if e.People[id].Wishlist == "" {
				mark = " (no wishlist yet)"
			}
			fmt.Fprintf(&sb, "• %s%s\n", mention(id), mark)
		}
		msg = sb.String()
		return nil
	})
	if err != nil {
		c.fail(err)
		return
	}
	c.public(msg)
}

func (b *Bot) draw(c *ctx) {
	var msgs []dm
	var count int
	err := b.store.Update(func() error {
		e, err := b.event(c.guild)
		if err != nil {
			return err
		}
		if !c.isOrganizer(e) {
			return userErr("Only the organizer (or someone with Manage Server) can draw names.")
		}
		if e.Drawn {
			return userErr("Names are already drawn. People can see theirs with `/santa recipient`.")
		}
		if len(e.Joined) < 3 {
			return userErr("You need at least 3 people, otherwise everyone knows who their Santa is.")
		}
		res, err := Draw(e.Joined, e.Excluded, rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64())))
		if errors.Is(err, ErrNoValidPairing) {
			return userErr("The exclusions make it impossible to pair everyone. Ask a few people to undo their `/santa exclude`.")
		}
		if err != nil {
			return err
		}
		e.Assignments = res
		e.FillRecipientUsernames()
		e.Drawn = true
		count = len(res)
		for giver, recipient := range res {
			msgs = append(msgs, dm{giver, assignmentText(e, recipient)})
		}
		return nil
	})
	if err != nil {
		c.fail(err)
		return
	}

	// DMs can take a while for big groups, and Discord wants a reply
	// within 3 seconds, so acknowledge first and fill in the result after.
	err = c.s.InteractionRespond(c.i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
	})
	if err != nil {
		log.Printf("defer: %v", err)
	}

	failed := sendDMs(c.s, msgs)
	out := fmt.Sprintf("🎄 **Names are drawn!** %d people have been DMed who they're buying for.\nLost it? `/santa recipient` shows it again (only to you).", count)
	if len(failed) > 0 {
		var who []string
		for _, id := range failed {
			who = append(who, mention(id))
		}
		out += "\n\n⚠️ I couldn't DM " + strings.Join(who, ", ") + " (DMs closed?). Use `/santa recipient` to see yours."
	}
	if _, err := c.s.InteractionResponseEdit(c.i.Interaction, &discordgo.WebhookEdit{Content: &out}); err != nil {
		log.Printf("edit response: %v", err)
	}
}

func assignmentText(e *Event, recipient string) string {
	p := e.People[recipient]
	var sb strings.Builder
	fmt.Fprintf(&sb, "🎅 **Secret Santa**: you're buying for **%s**!\n", p.Name)
	if e.Date != "" {
		fmt.Fprintf(&sb, "📅 Exchange: %s\n", e.Date)
	}
	if p.Wishlist != "" {
		fmt.Fprintf(&sb, "📝 Their wishlist:\n> %s\n", quote(p.Wishlist))
	} else {
		sb.WriteString("📝 They haven't written a wishlist yet. I'll DM you if they add one.\n")
	}
	sb.WriteString("\nSet your gift with `/santa gift` in the server: a message, up to 3 images, or both. I'll DM it to them, without your name, when the organizer ends the Secret Santa. Keep it secret! 🤫")
	return sb.String()
}

func (b *Bot) myRecipient(c *ctx) {
	var msg string
	err := b.store.View(func() error {
		e, err := b.event(c.guild)
		if err != nil {
			return err
		}
		if !e.Drawn {
			return userErr("Names haven't been drawn yet.")
		}
		r, ok := e.Assignments[c.user.ID]
		if !ok {
			return userErr("You're not part of this draw.")
		}
		msg = assignmentText(e, r)
		me := e.People[c.user.ID]
		if me.Gift == "" && len(me.Attachments) == 0 {
			msg += "\n\n🎁 You haven't set a gift yet."
			return nil
		}
		msg += "\n\n🎁 **Your gift** (delivered when the Secret Santa ends):" + quoted(me.Gift)
		if n := len(me.Attachments); n > 0 {
			msg += fmt.Sprintf("\n🖼️ %d image%s", n, plural(n))
		}
		return nil
	})
	if err != nil {
		c.fail(err)
		return
	}
	c.private(msg)
}

// maxGift keeps the delivered gift DM well under Discord's 2000-character
// message limit, leaving room for /santa recipient to show it with the wishlist.
const maxGift = 1000

func giftText(gift string) string {
	return "🎁 **Your Secret Santa sent you a gift!**" + quoted(gift)
}

// setGift stores the caller's gift (text, up to maxAttachments attachments, or
// both) for their recipient, replacing any earlier one. Nothing is sent now;
// end delivers every stored gift at once. There is no way for participants
// to message each other directly.
func (b *Bot) setGift(c *ctx, text string, atts []*discordgo.MessageAttachment) {
	if text == "" && len(atts) == 0 {
		c.private("Add a message, an image, or both.")
		return
	}
	// Counted after quoting, since each line break becomes "\n> " in the DM.
	if n := utf8.RuneCountInString(quote(text)); n > maxGift {
		c.private(fmt.Sprintf("Your message is %d characters; the limit is %d.", n, maxGift))
		return
	}

	// Check before downloading anything.
	var key string
	err := b.store.View(func() error {
		e, err := b.event(c.guild)
		if err != nil {
			return err
		}
		if _, ok := e.Assignments[c.user.ID]; !e.Drawn || !ok {
			return userErr("You don't have a recipient yet. Names haven't been drawn, or you're not in this draw.")
		}
		key = e.Key()
		return nil
	})
	if err != nil {
		c.fail(err)
		return
	}

	// Downloads happen outside the store lock so other commands (in this
	// server or any other) aren't held up.
	dir := filepath.Join(b.attachmentsDir, key)
	var files []string
	if len(atts) > 0 {
		c.deferReply(true) // downloading can take longer than Discord's 3-second reply window
		files, err = saveAttachments(dir, c.user.ID, atts)
		if err != nil {
			c.fail(err)
			return
		}
	}

	var name string
	var old []string
	err = b.store.Update(func() error {
		e, err := b.event(c.guild)
		if err != nil {
			return err
		}
		if e.Key() != key {
			return userErr("This Secret Santa ended while I was saving your gift.")
		}
		me := e.People[c.user.ID]
		old = me.Attachments
		me.Gift, me.Attachments = text, files
		me.Username = c.user.Username // usernames can change; record the current one with the gift
		e.FillRecipientUsernames()
		name = e.People[e.Assignments[c.user.ID]].Name
		return nil
	})
	if err != nil {
		removeFiles(dir, files)
		c.fail(err)
		return
	}
	removeFiles(dir, old)

	msg := fmt.Sprintf("🎁 Gift for **%s** saved ✅", name)
	if len(files) > 0 {
		msg += fmt.Sprintf(" (%d image%s)", len(files), plural(len(files)))
	}
	msg += "\nI'll DM it to them, without your name, when the organizer ends the Secret Santa. Running `/santa gift` again replaces it."
	c.private(msg)
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// quoted renders s as a block quote on its own line, or nothing if s is empty.
func quoted(s string) string {
	if s == "" {
		return ""
	}
	return "\n> " + quote(s)
}

func (b *Bot) remove(c *ctx, target *discordgo.User) {
	err := b.store.Update(func() error {
		e, err := b.event(c.guild)
		if err != nil {
			return err
		}
		if !c.isOrganizer(e) {
			return userErr("Only the organizer can remove people.")
		}
		if e.Drawn {
			return userErr("Names are already drawn. To start over, `/santa end` and create a new one.")
		}
		if target == nil || e.People[target.ID] == nil {
			return userErr("They're not in this Secret Santa.")
		}
		if target.ID == e.OrganizerID {
			return userErr("You can't remove the organizer; use `/santa end` instead.")
		}
		e.Remove(target.ID)
		return nil
	})
	if err != nil {
		c.fail(err)
		return
	}
	c.private(fmt.Sprintf("Removed %s.", mention(target.ID)))
}

// delivery is one gift to send when the round ends.
type delivery struct {
	to, text    string   // recipient and the DM they get
	attachments []string // attachment files in the round's folder
	santa       string   // who to return the gift to if it can't be delivered
	bounceMsg   string
}

// end backs up the round, deletes it, then delivers every saved gift to its
// recipient by DM. A gift that can't be delivered (recipient's DMs closed) is
// sent back to its Santa. Finally the round's attachments move into the backup.
func (b *Bot) end(c *ctx) {
	var gifts []delivery
	var players int
	var dir, backup string
	err := b.store.Update(func() error {
		e, err := b.event(c.guild)
		if err != nil {
			return err
		}
		if !c.isOrganizer(e) {
			return userErr("Only the organizer can end the Secret Santa.")
		}
		for santa, recipient := range e.Assignments {
			g := e.People[santa]
			if g == nil || (g.Gift == "" && len(g.Attachments) == 0) {
				continue
			}
			gifts = append(gifts, delivery{
				to: recipient, text: giftText(g.Gift), attachments: g.Attachments, santa: santa,
				bounceMsg: fmt.Sprintf("⚠️ I couldn't DM your gift to **%s** (their DMs are probably closed), so here it is back:%s",
					e.People[recipient].Name, quoted(g.Gift)),
			})
		}
		players = len(e.Assignments)
		dir = filepath.Join(b.attachmentsDir, e.Key())
		// Back up before deleting. If the backup can't be written, the round
		// stays as it is and the organizer sees an error, so nothing is lost.
		e.FillRecipientUsernames()
		backup, err = BackupEvent(b.backupsDir, e, time.Now())
		if err != nil {
			return fmt.Errorf("backing up round: %w", err)
		}
		delete(b.store.Events, c.guild)
		return nil
	})
	if err != nil {
		c.fail(err)
		return
	}

	c.deferReply(false) // delivering gifts can take longer than Discord's 3-second reply window

	var failed []string
	for _, g := range gifts {
		if err := sendGift(c.s, g.to, g.text, dir, g.attachments); err != nil {
			log.Printf("gift to %s failed: %v", g.to, err)
			failed = append(failed, g.to)
			if err := sendGift(c.s, g.santa, g.bounceMsg, dir, g.attachments); err != nil {
				log.Printf("returning gift to %s failed: %v", g.santa, err)
			}
		}
	}

	// Move this round's attachments in with its backup, keeping the same layout.
	if _, err := os.Stat(dir); err == nil {
		dest := filepath.Join(backup, "santa-attachments")
		if err := os.MkdirAll(dest, 0o700); err != nil {
			log.Printf("backup attachments folder: %v", err)
		} else if err := os.Rename(dir, filepath.Join(dest, filepath.Base(dir))); err != nil {
			log.Printf("moving %s into backup (left in place): %v", dir, err)
		}
	}
	log.Printf("round %s ended; backed up to %s", filepath.Base(dir), backup)

	out := "🎄 **This Secret Santa has ended.** Thanks for playing!"
	if players > 0 {
		out += fmt.Sprintf("\n🎁 %d of %d gifts were delivered by DM. Check your messages!", len(gifts)-len(failed), players)
	}
	if len(failed) > 0 {
		var who []string
		for _, id := range failed {
			who = append(who, mention(id))
		}
		out += "\n⚠️ I couldn't DM a gift to " + strings.Join(who, ", ") + " (DMs closed?). Their Santa got it back to pass on another way."
	}
	out += "\nStart a new one with `/santa create`."
	c.public(out)
}

// quote keeps multi-line text inside a Discord block quote.
func quote(s string) string { return strings.ReplaceAll(s, "\n", "\n> ") }
