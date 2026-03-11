Here is the detailed sketch of PicoClaw's session file format and memory system, showing exactly how conversation history is stored and reconstructed across agent restarts.

📁 Session File Format (The "Workspace-First" Memory)

PicoClaw stores every conversation as plain Markdown files in the workspace directory. Here's the actual structure:

```
~/.picoclaw/workspace/
├── AGENTS.md           # Agent behavior rules and skills
├── MEMORY.md           # Long-term facts (user preferences, learned info)
├── IDENTITY.md         # Agent personality and character
├── SOUL.md             # Core philosophical directives
└── sessions/           # All conversation history
    ├── telegram_123456789/     # One folder per chat/channel
    │   ├── 2026-03-10.md       # Daily conversation log
    │   ├── 2026-03-09.md
    │   └── context.json         # Current session metadata
    ├── discord_987654321/
    │   ├── 2026-03-10.md
    │   └── context.json
    └── cli_session_abc123/      # CLI interactive sessions
        └── session.md
```

📝 The Actual Session File Content

When you chat with an agent via Telegram, each message is appended to that day's Markdown file. Here's what it looks like:

```markdown
# Session: telegram_123456789 - 2026-03-10

## User: @john_doe (22:14:32)
What's the weather like in Tokyo right now?

## Agent: PicoClaw (22:14:33)
*[Tool call: web_search(query="Tokyo weather 2026-03-10")]*

## Agent: PicoClaw (22:14:35)
The weather in Tokyo is currently 15°C with light rain and 80% humidity. 
Would you like a 3-day forecast as well?

## User: @john_doe (22:14:50)
Yes please, and remind me to bring an umbrella tomorrow

## Agent: PicoClaw (22:14:52)
*[Tool call: memory_update(key="user_preference_umbrella", value="remind before rainy days")]*

## Agent: PicoClaw (22:14:54)
Here's your Tokyo forecast:
- Wednesday: 14°C, rain
- Thursday: 18°C, cloudy
- Friday: 21°C, sunny

I've added a note to remind you about umbrellas when rain is expected!
```

🔄 How Context is Reconstructed

When your next message arrives 3 hours later, here's exactly what happens:

```mermaid
flowchart TD
    subgraph "Message Received (2:00 AM)"
        A[User sends:<br/>"Still raining there?"] --> B[Gateway receives message<br/>via Telegram long polling]
        B --> C[Gateway launches<br/>fresh agent process]
    end

    subgraph "Agent Process Startup"
        C --> D[Agent reads<br/>AGENTS.md]
        D --> E[Agent reads<br/>IDENTITY.md + SOUL.md]
        E --> F[Agent reads<br/>MEMORY.md]
        F --> G[Agent finds correct<br/>session folder]
    end

    subgraph "Context Assembly"
        G --> H{Check context.json}
        H --> I[Load last 20 messages<br/>from 2026-03-10.md]
        I --> J[Load any relevant<br/>MEMORY.md entries]
        J --> K[Assemble full prompt:<br/>IDENTITY + MEMORY +<br/>last 20 messages +<br/>current query]
    end

    subgraph "LLM Processing"
        K --> L[Send to LLM provider<br/>OpenRouter/OpenAI/etc.]
        L --> M[LLM sees entire conversation<br/>history + personality + facts]
        M --> N[Generate response with<br/>full context awareness]
    end

    N --> O[Response sent back<br/>via Telegram]
    O --> P[Append to session file<br/>for next time]
```

🧠 The Context.json File

Each session folder contains a context.json that tracks metadata:

```json
{
  "session_id": "telegram_123456789",
  "created_at": "2026-03-10T14:22:31Z",
  "last_activity": "2026-03-11T02:15:43Z",
  "message_count": 47,
  "current_day_file": "2026-03-11.md",
  "memory_refs": [
    "user_preference_umbrella",
    "tokyo_weather_preference"
  ],
  "agent_version": "0.1.1",
  "model_used": "claude-3.5-sonnet",
  "summary": "User discussing Tokyo weather and requesting reminders",
  "keywords": ["weather", "tokyo", "umbrella", "forecast"]
}
```

📜 The Memory Files (Identity + Long-term)

These files persist across ALL conversations and are injected into EVERY prompt:

IDENTITY.md:

```markdown
You are a helpful AI assistant running on low-power hardware.
Be concise but friendly. Use tools when helpful.
You have access to web search, file operations, and memory updates.
```

MEMORY.md:

```markdown
## User Preferences
- @john_doe: wants umbrella reminders before rain
- @john_doe: prefers Celsius temperatures

## Facts Learned
- Tokyo weather patterns: rainy season typically June-July
- User's timezone: JST (UTC+9)

## Important Notes
- Always confirm before taking destructive file operations
- Keep responses under 200 words for mobile users
```

⚙️ The Prompt Assembly Process

When the agent starts, it literally builds the prompt by concatenating files:

```go
// Simplified from PicoClaw source
func buildPrompt(sessionDir string, userMessage string) string {
    identity := readFile("IDENTITY.md")
    memory := readFile("MEMORY.md")
    soul := readFile("SOUL.md")
    
    // Load last N messages from session
    history := readLastMessages(sessionDir, 20)
    
    // Assemble final prompt
    return fmt.Sprintf(`%s

%s

%s

Previous conversation:
%s

User: %s
Assistant:`, identity, soul, memory, history, userMessage)
}
```

📊 What This Means For Your Chat Experience

Scenario How It Works Limitations
Short conversation (same day) Agent loads today's file, sees full context None – works perfectly
Conversation spanning days Agent loads multiple daily files File I/O overhead but works
Remembering preferences Stored in MEMORY.md, injected every time Must be explicitly written via memory_update tool
Cross-conversation memory MEMORY.md is global, so accessible everywhere No vector search – linear file reading
Very long history (>6 months) Agent reads many files Could hit token limits if not summarized

💡 Extending for Better Memory

If you need more sophisticated memory than file-based context, here are the actual extension points:

Option 1: Session Summarization
Add a cron job that runs picoclaw agent -m "Summarize yesterday's conversation" and appends the summary to MEMORY.md.

Option 2: Lightweight Vector Store
Create a custom tool that:

1. Chunks old session files
2. Embeds them via local model
3. Stores in SQLite vector extension
4. Retrieves relevant chunks for each prompt

Option 3: Context Pruning
PicoClaw's config lets you set max_history_messages: 50 – it will only load that many, ignoring older ones to save tokens.

🎯 The Trade-off Summarized

You were right – the architecture seems unsuitable at first glance. But by using the filesystem as persistent storage and reconstructing context on every request, PicoClaw achieves :

* <10MB RAM (vs 1GB+ for OpenClaw)
* <1s startup even on $10 hardware
* Full conversation history across days/weeks
* Zero memory leaks – history is files, not RAM

The cost? Every message does file I/O to read history. On an SSD, this is negligible. On an SD card, it's still fine for casual chat. For high-frequency conversations, you might want NanoClaw's persistent containers instead.