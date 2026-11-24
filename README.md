# Secret Santa Discord bot

One slash command, `/santa`, with everything as subcommands. Written in Go on
[discordgo](https://github.com/bwmarrin/discordgo). State is a single JSON file.

## Setup

1. **Create the bot**: <https://discord.com/developers/applications> → New Application →
   *Bot* → Reset Token (copy it). No privileged intents needed.
2. **Invite it**: *OAuth2 → URL Generator*, tick scopes `bot` + `applications.commands`,
   permission `Send Messages`. Open the URL, pick your server.
3. **Build** (dependency source is vendored into the project, built locally):
   ```sh
   go mod tidy       # writes go.sum
   go mod vendor     # copies discordgo + its deps into ./vendor
   go test ./...
   go build -o santa .
   ```
4. **Run**:
   ```sh
   DISCORD_TOKEN=... ./santa
   ```
   | Env var | Default | Meaning |
   |---|---|---|
   | `DISCORD_TOKEN` | (required) | Bot token |
   | `SANTA_DATA` | `santa.json` | Where state is saved |
   | `SANTA_ATTACHMENTS` | `santa-attachments` | Gift attachments waiting for `/santa end` |
   | `SANTA_BACKUPS` | `santa-backups` | One folder per ended round |
   | `GUILD_ID` | (global) | Register commands to one server only; handy while developing |

## Commands

| Command | Who | What |
|---|---|---|
| `/santa create [date]` | admin | Start a round in this server (one per server). Creator is the organizer and auto-joins |
| `/santa join [wishlist]` | anyone | Join before the draw |
| `/santa leave` | participant | Leave before the draw |
| `/santa wishlist <text>` | participant | Set wishlist. After the draw, your Santa gets a DM when it changes |
| `/santa exclude @user` | participant | Toggle "never pair me with them" (both directions), e.g. partners |
| `/santa list` | anyone | Who's in, who's missing a wishlist |
| `/santa draw` | organizer | Draw names and DM each person who they're buying for |
| `/santa recipient` | participant | Show your recipient again, plus their wishlist and the gift you've set (only you see it) |
| `/santa gift [text] [attachment1] [attachment2] [attachment3]` | participant | Set your gift: a message, up to 3 attachments, or both. Running it again replaces the whole gift. Nothing is sent yet |
| `/santa remove @user` | organizer | Remove someone before the draw |
| `/santa end` | organizer | DM every saved gift to its recipient (anonymously), then delete the round |

"Admin" means anyone with the *Administrator* or *Manage Server* permission.
"Organizer" means whoever ran `create`, or any admin.

Participants can't message each other through the bot. Gifts are held until the
organizer runs `/santa end`, then each one is DMed to its recipient without the
Santa's name. If a recipient has DMs closed, their gift is DMed back to their Santa.

## Gift attachments (`attachments.go`, `metadata.go`)

- JPEG, PNG, GIF or WebP images, up to 3 per gift and 8 MB each. That's below every upload limit
  Discord has had for bots; the current limit depends on the server.
- The bot downloads its own copy right away, because Discord's links to
  uploaded files expire after about a day.
- **Hidden photo info is removed before saving**: GPS location, phone model,
  dates, author names, comments, and anything else that isn't pixels or color.
  This is done without re-encoding, so quality doesn't change. The "rotate
  this photo" flag is kept so phone photos don't arrive sideways.
- Delivered as `gift-1.jpg`, `gift-2.png`… so the Santa's filenames aren't shown.

## Backups

`/santa end` saves the round to `santa-backups/<date>_<time>-<serverID>/`
**before** deleting it. If the backup can't be written, the round is not
ended. The folder contains:

- `santa.json`: only this round (who drew whom, wishlists, gift text), in the
  same format as the main file
- `santa-attachments/`: the round's gift attachments, moved there after delivery

Backups contain gift messages and attachments and are never deleted
automatically. Delete old ones yourself when you no longer need them.

## Several servers at once

One running bot can serve any number of servers. `santa.json` holds every
server's round, keyed by server ID, and each server can have one round at a
time. Commands only touch the round of the server they're typed in, and
someone in two servers is in two separate rounds. Every change goes through a
single lock, so two servers saving at the same moment can't overwrite each
other. Slow work (downloading attachments, sending DMs) happens outside that lock,
so a big `/santa end` in one server doesn't hold up commands in another.

Run **one** copy of the bot per `santa.json`. Two copies sharing a file would
each keep their own copy in memory and overwrite each other's saves.

## How the draw works (`pairing.go`)

It first looks for **one big loop** (A→B→C→…→A) that respects exclusions, so no two
people just swap gifts (which would let them work out who their Santa is). If
exclusions make a single loop impossible, it falls back to any valid assignment.
Both are randomized backtracking searches; `pairing_test.go` covers the edge cases
(no self-draws, exclusions honored, fallback, impossible setups).
