package main

import (
    "bufio"
    "context"
    "fmt"
    "io/fs"
    "log"
    "os"
    "os/signal"
    "path/filepath"
    "strings"
    "sync"
    "sync/atomic"
    "time"

    "github.com/cloudflare/ahocorasick"
    "github.com/go-telegram/bot"
    "github.com/go-telegram/bot/models"
)


// PendingPoll 保存每个新用户的投票状态
type PendingPoll struct {
    UserID          int64
    ChatID          int64
    Username        string
    PollMessageID   int
    NoticeMessageID int
    Timer           *time.Timer
    Voted           bool
}

var (
    pendingPolls = make(map[int64]*PendingPoll)
    pollsMu      sync.RWMutex
    wg           sync.WaitGroup

    acMatcher    atomic.Value // Aho-Corasick 自动机
)

// safeGo 启动 goroutine 并 recover panic
func safeGo(f func()) {
    wg.Add(1)
    go func() {
        defer wg.Done()
        defer func() {
            if r := recover(); r != nil {
                log.Printf("[panic recovered] %v", r)
            }
        }()
        f()
    }()
}

func boolPtr(b bool) *bool { return &b }

// retry 带简单指数退避重试
func retry(attempts int, initialSleep time.Duration, fn func() error) error {
    sleep := initialSleep
    for i := 0; i < attempts; i++ {
        if err := fn(); err == nil {
            return nil
        } else if i == attempts-1 {
            return err
        }
        time.Sleep(sleep)
        sleep *= 2
    }
    return nil
}

// 封禁发言（限制所有权限）
func restrictUser(ctx context.Context, b *bot.Bot, chatID, userID int64) error {
    _, err := b.RestrictChatMember(ctx, &bot.RestrictChatMemberParams{
        ChatID: chatID,
        UserID: userID,
        Permissions: &models.ChatPermissions{
            CanSendMessages:       false,
            CanSendAudios:         false,
            CanSendDocuments:      false,
            CanSendPhotos:         false,
            CanSendVideos:         false,
            CanSendVideoNotes:     false,
            CanSendVoiceNotes:     false,
            CanSendPolls:          false,
            CanSendOtherMessages:  false,
            CanAddWebPagePreviews: false,
            CanChangeInfo:         false,
            CanInviteUsers:        false,
            CanPinMessages:        false,
            CanManageTopics:       false,
        },
    })
    return err
}

// 解除禁言（恢复全部权限）
func unrestrictUser(ctx context.Context, b *bot.Bot, chatID, userID int64) error {
    _, err := b.RestrictChatMember(ctx, &bot.RestrictChatMemberParams{
        ChatID: chatID,
        UserID: userID,
        Permissions: &models.ChatPermissions{
            CanSendMessages:       true,
            CanSendAudios:         true,
            CanSendDocuments:      true,
            CanSendPhotos:         true,
            CanSendVideos:         true,
            CanSendVideoNotes:     true,
            CanSendVoiceNotes:     true,
            CanSendPolls:          false,
            CanSendOtherMessages:  true,
            CanAddWebPagePreviews: false,
            CanChangeInfo:         false,
            CanInviteUsers:        false,
            CanPinMessages:        false,
            CanManageTopics:       false,
        },
    })
    return err
}

// 封禁用户（带重试）
func banUserWithRetry(ctx context.Context, b *bot.Bot, chatID, userID int64, duration time.Duration) error {
    return retry(3, 500*time.Millisecond, func() error {
        _, err := b.BanChatMember(ctx, &bot.BanChatMemberParams{
            ChatID:        chatID,
            UserID:        userID,
            UntilDate:     int(time.Now().Add(duration).Unix()),
            RevokeMessages: true,
        })
        return err
    })
}

// 删除消息（带重试）
func deleteMessageWithRetry(ctx context.Context, b *bot.Bot, chatID int64, messageID int) error {
    return retry(3, 300*time.Millisecond, func() error {
        _, err := b.DeleteMessage(ctx, &bot.DeleteMessageParams{
            ChatID:    chatID,
            MessageID: messageID,
        })
        return err
    })
}

// 发送投票（带重试）
func sendPollWithRetry(ctx context.Context, b *bot.Bot, chatID int64, question string, options []models.InputPollOption, anonymous bool, openPeriod int) (*models.Message, error) {
    var msg *models.Message
    err := retry(3, 300*time.Millisecond, func() error {
        m, err := b.SendPoll(ctx, &bot.SendPollParams{
            ChatID:      chatID,
            Question:    question,
            Options:     options,
            IsAnonymous: boolPtr(anonymous),
            OpenPeriod:  openPeriod,
        })
        if err == nil {
            msg = m
        }
        return err
    })
    return msg, err
}

// 安全操作 pendingPolls
func setPending(p *PendingPoll) {
    pollsMu.Lock()
    defer pollsMu.Unlock()
    pendingPolls[p.UserID] = p
}

func getPending(userID int64) (*PendingPoll, bool) {
    pollsMu.RLock()
    defer pollsMu.RUnlock()
    p, ok := pendingPolls[userID]
    return p, ok
}

func deletePending(userID int64) {
    pollsMu.Lock()
    p, ok := pendingPolls[userID]
    if ok {
        if p.Timer != nil {
            p.Timer.Stop()
            p.Timer = nil
        }
        delete(pendingPolls, userID)
    }
    pollsMu.Unlock()
}

// 删除投票和提示消息
func cleanupPending(ctx context.Context, b *bot.Bot, p *PendingPoll) {
    if p.PollMessageID != 0 {
        _ = deleteMessageWithRetry(ctx, b, p.ChatID, p.PollMessageID)
    }
    if p.NoticeMessageID != 0 {
        _ = deleteMessageWithRetry(ctx, b, p.ChatID, p.NoticeMessageID)
    }
    deletePending(p.UserID)
}

// =====================
// Aho-Corasick 关键词匹配
// =====================
// loadKeywordsFolderHot 从指定文件夹读取所有 .txt 文件，并合并关键词
func loadKeywordsFolderHot(folder string) []string {
    info, err := os.Stat(folder)
    if err != nil || !info.IsDir() {
        log.Printf("⚠️ 无法访问文件夹 %s: %v", folder, err)
        return nil
    }

    var keywords []string
    filepath.WalkDir(folder, func(path string, d fs.DirEntry, err error) error {
        if err != nil || d.IsDir() || !strings.HasSuffix(strings.ToLower(d.Name()), ".txt") {
            return nil
        }

        f, err := os.Open(path)
        if err != nil {
            log.Printf("⚠️ 打开文件 %s 失败: %v", path, err)
            return nil
        }
        defer f.Close()

        scanner := bufio.NewScanner(f)
        for scanner.Scan() {
            line := strings.TrimSpace(scanner.Text())
            if line != "" {
                keywords = append(keywords, strings.ToLower(line))
            }
        }
        if err := scanner.Err(); err != nil {
            log.Printf("⚠️ 读取文件 %s 失败: %v", path, err)
        }
        return nil
    })

    if len(keywords) == 0 {
        log.Printf("⚠️ 文件夹 %s 下没有读取到任何关键词", folder)
    } else {
        log.Printf("🔑 已加载 %d 个关键词", len(keywords))
    }

    return keywords
}

// startACHotReload 启动热更新 goroutine，每 interval 扫描一次文件夹
func startACHotReload(folder string, interval time.Duration) {
    // 启动前确保文件夹存在
    if _, err := os.Stat(folder); os.IsNotExist(err) {
        log.Printf("⚠️ 文件夹 %s 不存在，自动创建", folder)
        if err := os.MkdirAll(folder, 0755); err != nil {
            log.Fatalf("创建文件夹 %s 失败: %v", folder, err)
        }
    }

    // 初始化一个空自动机（空关键词切片）避免 atomic.Store(nil) panic
    acMatcher.Store(ahocorasick.NewStringMatcher([]string{}))

    go func() {
        for {
            keywords := loadKeywordsFolderHot(folder)
            if len(keywords) > 0 {
                newMatcher := ahocorasick.NewStringMatcher(keywords)
                acMatcher.Store(newMatcher) // 原子替换，无锁读取
            } else {
                // 空切片生成空自动机，不再存 nil
                acMatcher.Store(ahocorasick.NewStringMatcher([]string{}))
            }
            time.Sleep(interval)
        }
    }()
}

// extractTextFromMessage 提取消息里的所有可检测文本
func extractTextFromMessage(msg *models.Message) string {
    var parts []string

    if msg == nil {
        return ""
    }

    // 纯文本消息
    if msg.Text != "" {
        parts = append(parts, msg.Text)
    }

    // 图片/视频/文档/音频/动画的 Caption
    if msg.Caption != "" {
        parts = append(parts, msg.Caption)
    }

    // 其他可能的字段，未来可扩展
    // if msg.Sticker != nil && msg.Sticker.Emoji != "" { parts = append(parts, msg.Sticker.Emoji) }
    // if msg.Game != nil && msg.Game.Title != "" { parts = append(parts, msg.Game.Title) }

    return strings.Join(parts, "\n")
}


// containsAnyKeywordAC 判断文本是否包含敏感词（无锁）
func containsAnyKeywordAC(text string) bool {
    if text == "" {
        return false
    }
    v := acMatcher.Load()
    if v == nil {
        return false
    }
    matcher := v.(*ahocorasick.Matcher)
    matches := matcher.Match([]byte(strings.ToLower(text)))
    return len(matches) > 0
}

// initAC 初始化自动机（启动热更新）
func initAC() {
    startACHotReload("keywords", 5*time.Second)
}

// =====================
// 默认 handler
// =====================
func defaultHandler(ctx context.Context, b *bot.Bot, update *models.Update) {
    defer func() {
        if r := recover(); r != nil {
            log.Printf("[handler panic recovered] %v", r)
        }
    }()

    // 消息处理
if update.Message != nil {
    msg := update.Message
    chatID := msg.Chat.ID
    var userID int64
    var userName string
    if msg.From != nil {
        userID = msg.From.ID
        userName = msg.From.FirstName
        if msg.From.LastName != "" {
            userName += " " + msg.From.LastName
        }
    } else {
        userID = chatID
        userName = fmt.Sprintf("%d", chatID)
    }

    // ====== 转发消息检测优先 ======
    forwardDetected := false

    if msg.ForwardOrigin != nil {
        // 根据 Type 判断
        switch msg.ForwardOrigin.Type {
        case "user", "hidden_user", "chat", "channel":
            forwardDetected = true
        }
    }

    if msg.IsAutomaticForward {
        forwardDetected = true
    }

    if forwardDetected {
        // 删除消息
        _ = deleteMessageWithRetry(ctx, b, chatID, msg.ID)
        log.Printf("🚫 已删除用户 %d 的转发消息", userID)

        // 发送提醒
        warnText := fmt.Sprintf("<a href=\"tg://user?id=%d\">%s</a>：请注意，禁止批量转发消息。", userID, userName)
        warnMsg, err := b.SendMessage(ctx, &bot.SendMessageParams{
            ChatID:    chatID,
            Text:      warnText,
            ParseMode: models.ParseModeHTML,
        })
        if err == nil {
            time.AfterFunc(60*time.Second, func() {
                safeGo(func() { _ = deleteMessageWithRetry(ctx, b, chatID, warnMsg.ID) })
            })
        }
        return
    }

    // ====== 敏感关键词检测 ======
    content := extractTextFromMessage(msg)
    if containsAnyKeywordAC(content) {
        _ = deleteMessageWithRetry(ctx, b, chatID, msg.ID)
        log.Printf("🚫 已删除用户 %d 的敏感关键词消息", userID)
        return
    }
}

    // ==============================
    // 加入验证逻辑
    // ==============================
    if update.ChatJoinRequest != nil {
        req := update.ChatJoinRequest
        chatID := req.Chat.ID
        userID := req.From.ID
        username := req.From.Username

        if username == "" {
            _, _ = b.DeclineChatJoinRequest(ctx, &bot.DeclineChatJoinRequestParams{
                ChatID: chatID,
                UserID: userID,
            })
            log.Printf("🚫 已拒绝无用户名用户 user=%d", userID)
            return
        }

        ok, err := b.ApproveChatJoinRequest(ctx, &bot.ApproveChatJoinRequestParams{
            ChatID: chatID,
            UserID: userID,
        })
        if err != nil || !ok {
            log.Printf("批准用户失败 user=%d err=%v", userID, err)
            return
        }

        // 默认禁言
        _ = restrictUser(ctx, b, chatID, userID)

        // 发送提示消息
        noticeMsg, err := b.SendMessage(ctx, &bot.SendMessageParams{
            ChatID:    chatID,
            Text:      fmt.Sprintf("<a href=\"tg://user?id=%d\">%s</a>请进行验证（60秒内），如果验证失败可以稍后重试", userID, username),
            ParseMode: models.ParseModeHTML,
        })
        if err != nil {
            return
        }

        // 发非匿名投票
        options := []models.InputPollOption{{Text: "✅ 验证"}, {Text: "❌ 拒绝"}}
        pollMsg, err := sendPollWithRetry(ctx, b, chatID, "请选择验证选项", options, false, 60)
        if err != nil {
            _ = deleteMessageWithRetry(ctx, b, chatID, noticeMsg.ID)
            return
        }

        // 保存 pending
        p := &PendingPoll{
            UserID:          userID,
            ChatID:          chatID,
            Username:        username,
            PollMessageID:   pollMsg.ID,
            NoticeMessageID: noticeMsg.ID,
        }
        setPending(p)

        // 启动 60s 计时器
        timer := time.AfterFunc(60*time.Second, func() {
            safeGo(func() {
                pending, ok := getPending(userID)
                if !ok || pending.Voted {
                    return
                }
                _ = banUserWithRetry(ctx, b, pending.ChatID, pending.UserID, 1*time.Minute)
                cleanupPending(ctx, b, pending)
            })
        })
        pollsMu.Lock()
        if p2, ok := pendingPolls[userID]; ok {
            p2.Timer = timer
            pendingPolls[userID] = p2
        }
        pollsMu.Unlock()
    }

    // PollAnswer 处理
    if update.PollAnswer != nil {
        answer := update.PollAnswer
        user := answer.User
        if user == nil {
            return
        }
        pollUserID := user.ID
        p, ok := getPending(pollUserID)
        if !ok {
            return
        }

        chosenAccept := false
        for _, optID := range answer.OptionIDs {
            if optID == 0 {
                chosenAccept = true
                break
            }
        }

        if chosenAccept {
            pollsMu.Lock()
            if p.Timer != nil {
                p.Timer.Stop()
                p.Timer = nil
            }
            p.Voted = true
            pollsMu.Unlock()

            safeGo(func() {
                cleanupPending(ctx, b, p)
                _ = unrestrictUser(ctx, b, p.ChatID, p.UserID)
                log.Printf("✅ 用户 %s(%d) 验证通过", p.Username, p.UserID)
            })
            return
        }

        // 拒绝处理
        for _, optID := range answer.OptionIDs {
            if optID == 1 {
                pollsMu.Lock()
                if p.Timer != nil {
                    p.Timer.Stop()
                    p.Timer = nil
                }
                pollsMu.Unlock()

                safeGo(func() {
                    _ = banUserWithRetry(ctx, b, p.ChatID, p.UserID, 1*time.Minute)
                    cleanupPending(ctx, b, p)
                    log.Printf("❌ 用户 %s(%d) 投票拒绝，已封禁 1 分钟", p.Username, p.UserID)
                })
                return
            }
        }
    }
}

func main() {
    initAC() // 初始化 Aho-Corasick

    ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
    defer cancel()

    token := os.Getenv("TELEGRAM_BOT_TOKEN")
    if token == "" {
        log.Fatal("请先设置环境变量 TELEGRAM_BOT_TOKEN")
    }

    b, err := bot.New(
        token,
        bot.WithDefaultHandler(defaultHandler),
        bot.WithAllowedUpdates([]string{"chat_join_request", "poll_answer", "poll", "message"}),
    )
    if err != nil {
        log.Fatalf("bot.New error: %v", err)
    }

    log.Println("Bot 已启动")
    safeGo(func() { b.Start(ctx) })

    <-ctx.Done()
    log.Println("收到退出信号，等待异步任务完成...")
    wg.Wait()
    log.Println("已干净退出")
}
