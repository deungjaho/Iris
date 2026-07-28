package wechat

// ilink API 客户端——借用 @tencent-weixin/openclaw-weixin 的协议
// 端点：https://ilinkai.weixin.qq.com/ilink/bot/{getupdates,sendmessage,...}

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"
)

const (
	ILinkAppID            = "bot"
	ILinkAppClientVersion = 0x00020406 // 2.4.6 -> (2<<16)|(4<<8)|6 = 132102
	DefaultLongPoll       = 35 * time.Second
	DefaultAPITimeout     = 15 * time.Second
)

// Account 凭证（从 OpenClaw 的 ~/.openclaw/openclaw-weixin/ 复用）
type Account struct {
	Token         string `json:"token"`
	BaseURL       string `json:"baseUrl"`
	UserID        string `json:"userId"`
	ContextToken  string `json:"contextToken,omitempty"`
	GetUpdatesBuf string `json:"getUpdatesBuf,omitempty"`
}

// MessageItem 类型
const (
	ItemText  = 1
	ItemImage = 2
	ItemVoice = 3
	ItemFile  = 4
	ItemVideo = 5
)

const (
	MsgUser = 1
	MsgBot  = 2

	MsgStateFinish = 2
)

// InboundMessage 从 getupdates 收到的消息
type InboundMessage struct {
	FromUserID    string        `json:"from_user_id"`
	ToUserID      string        `json:"to_user_id"`
	ItemList      []InboundItem `json:"item_list"`
	ContextToken  string        `json:"context_token,omitempty"`
	SessionID     string        `json:"session_id,omitempty"`
	MessageID     json.Number   `json:"message_id,omitempty"`
	Seq           json.Number   `json:"seq,omitempty"`
	CreateTimeMs  json.Number   `json:"create_time_ms,omitempty"`
}

type InboundItem struct {
	Type     int       `json:"type"`
	TextItem *TextItem `json:"text_item,omitempty"`
}

type TextItem struct {
	Text string `json:"text"`
}

// GetUpdatesResp
type GetUpdatesResp struct {
	Ret           int             `json:"ret"`
	Errmsg        string          `json:"errmsg,omitempty"`
	Msgs          []InboundMessage `json:"msgs,omitempty"`
	GetUpdatesBuf string          `json:"get_updates_buf,omitempty"`
	SyncBuf       string          `json:"sync_buf,omitempty"`
}

// Client ilink API 客户端
type Client struct {
	account Account
	http    *http.Client
}

func NewClient(acc Account) *Client {
	// 用默认 Transport（走系统代理，omarchy 需要代理访问外网）
	return &Client{
		account: acc,
		http: &http.Client{
			Timeout:   DefaultLongPoll + 10*time.Second,
			Transport: http.DefaultTransport,
		},
	}
}

func (c *Client) UpdateBuf(buf string) { c.account.GetUpdatesBuf = buf }
func (c *Client) UpdateContextToken(t string) {
	if t != "" {
		c.account.ContextToken = t
	}
}
func (c *Client) Account() Account { return c.account }

// randomWechatUin: random uint32 -> decimal string -> base64
func randomWechatUin() string {
	var b [4]byte
	rand.Read(b[:])
	v := binary.BigEndian.Uint32(b[:])
	return base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("%d", v)))
}

func (c *Client) commonHeaders() http.Header {
	h := http.Header{}
	h.Set("iLink-App-Id", ILinkAppID)
	h.Set("iLink-App-ClientVersion", fmt.Sprintf("%d", ILinkAppClientVersion))
	return h
}

func (c *Client) postHeaders() http.Header {
	h := c.commonHeaders()
	h.Set("Content-Type", "application/json")
	h.Set("AuthorizationType", "ilink_bot_token")
	h.Set("X-WECHAT-UIN", randomWechatUin())
	if c.account.Token != "" {
		h.Set("Authorization", "Bearer "+c.account.Token)
	}
	return h
}

type baseInfo struct {
	ChannelVersion string `json:"channel_version"`
	BotAgent       string `json:"bot_agent"`
}

func baseInfoObj() baseInfo {
	return baseInfo{ChannelVersion: "2.4.6", BotAgent: "Iris/0.1"}
}

// GetUpdates 长轮询拉消息
func (c *Client) GetUpdates(ctx context.Context) (*GetUpdatesResp, error) {
	body := map[string]any{
		"get_updates_buf": c.account.GetUpdatesBuf,
		"base_info":       baseInfoObj(),
	}
	bodyBytes, _ := json.Marshal(body)

	req, err := http.NewRequestWithContext(ctx, "POST",
		c.account.BaseURL+"/ilink/bot/getupdates",
		bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}
	req.Header = c.postHeaders()

	resp, err := c.http.Do(req)
	if err != nil {
		// 长轮询超时或 ctx 取消都算正常
		return &GetUpdatesResp{Ret: 0, Msgs: []InboundMessage{}, GetUpdatesBuf: c.account.GetUpdatesBuf}, nil
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("getupdates %d: %s", resp.StatusCode, string(raw))
	}

	var r GetUpdatesResp
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("getupdates parse: %w raw=%s", err, string(raw))
	}
	if r.Ret != 0 {
		return nil, fmt.Errorf("getupdates ret=%d errmsg=%s", r.Ret, r.Errmsg)
	}
	if r.Msgs == nil {
		r.Msgs = []InboundMessage{}
	}
	if r.GetUpdatesBuf != "" {
		c.account.GetUpdatesBuf = r.GetUpdatesBuf
	}
	return &r, nil
}

// SendMessage 发文本消息（自动转义下划线）
func (c *Client) SendMessage(ctx context.Context, toUserID, text string) error {
	// 转义下划线，避免微信 markdown 渲染吞噬
	text = EscapeMarkdown(text)
	itemList := []map[string]any{}
	if text != "" {
		itemList = append(itemList, map[string]any{
			"type":      ItemText,
			"text_item": map[string]any{"text": text},
		})
	}
	body := map[string]any{
		"msg": map[string]any{
			"from_user_id":   "",
			"to_user_id":     toUserID,
			"client_id":      fmt.Sprintf("iris-%d", time.Now().UnixNano()),
			"message_type":   MsgBot,
			"message_state":  MsgStateFinish,
			"item_list":      itemList,
			"context_token":  c.account.ContextToken,
		},
		"base_info": baseInfoObj(),
	}
	bodyBytes, _ := json.Marshal(body)

	req, err := http.NewRequestWithContext(ctx, "POST",
		c.account.BaseURL+"/ilink/bot/sendmessage",
		bytes.NewReader(bodyBytes))
	if err != nil {
		return err
	}
	req.Header = c.postHeaders()

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return fmt.Errorf("sendmessage %d: %s", resp.StatusCode, string(raw))
	}

	var r struct {
		Ret    int    `json:"ret"`
		Errmsg string `json:"errmsg,omitempty"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return fmt.Errorf("sendmessage parse: %w", err)
	}
	if r.Ret != 0 {
		return fmt.Errorf("sendmessage ret=%d errmsg=%s", r.Ret, r.Errmsg)
	}
	return nil
}

// SendTyping 发"正在输入"状态
func (c *Client) SendTyping(ctx context.Context, toUserID string) error {
	body := map[string]any{
		"to_user_id": toUserID,
		"typing_status": 1,
		"base_info":   baseInfoObj(),
	}
	bodyBytes, _ := json.Marshal(body)

	req, err := http.NewRequestWithContext(ctx, "POST",
		c.account.BaseURL+"/ilink/bot/sendtyping",
		bytes.NewReader(bodyBytes))
	if err != nil {
		return err
	}
	req.Header = c.postHeaders()

	resp, err := c.http.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	return nil
}

// ExtractText 从 InboundMessage 提取文本
func (m *InboundMessage) ExtractText() string {
	for _, item := range m.ItemList {
		if item.Type == ItemText && item.TextItem != nil {
			return item.TextItem.Text
		}
	}
	return ""
}

// NotifyStart 通知微信端 bot 上线
func (c *Client) NotifyStart(ctx context.Context) error {
	body := map[string]any{"base_info": baseInfoObj()}
	bodyBytes, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, "POST",
		c.account.BaseURL+"/ilink/bot/msg/notifystart",
		bytes.NewReader(bodyBytes))
	if err != nil {
		return err
	}
	req.Header = c.postHeaders()
	resp, err := noProxyClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	log.Printf("notifyStart: status=%d body=%s", resp.StatusCode, string(raw))
	return nil
}

// NotifyStop 通知微信端 bot 下线
func (c *Client) NotifyStop(ctx context.Context) error {
	body := map[string]any{"base_info": baseInfoObj()}
	bodyBytes, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, "POST",
		c.account.BaseURL+"/ilink/bot/msg/notifystop",
		bytes.NewReader(bodyBytes))
	if err != nil {
		return err
	}
	req.Header = c.postHeaders()
	resp, err := noProxyClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.ReadAll(resp.Body)
	return nil
}

// === 登录流程 ===

// QRCodeResp get_bot_qrcode 响应
type QRCodeResp struct {
	Ret              int    `json:"ret"`
	QRCode           string `json:"qrcode"`
	QRCodeImgContent string `json:"qrcode_img_content"`
}

// QRStatusResp get_qrcode_status 响应
type QRStatusResp struct {
	Status       string `json:"status"`
	BotToken     string `json:"bot_token"`
	IlinkBotID   string `json:"ilink_bot_id"`
	BaseURL      string `json:"baseurl"`
	IlinkUserID  string `json:"ilink_user_id"`
}

// noProxyClient 走系统代理的 client（omarchy 需要代理访问外网）
var noProxyClient = &http.Client{
	Timeout:   30 * time.Second,
	Transport: http.DefaultTransport,
}

// FetchQRCode 请求登录二维码
func FetchQRCode(ctx context.Context, baseURL string, localTokens []string) (*QRCodeResp, error) {
	body := map[string]any{"local_token_list": localTokens}
	bodyBytes, _ := json.Marshal(body)

	req, err := http.NewRequestWithContext(ctx, "POST",
		baseURL+"/ilink/bot/get_bot_qrcode?bot_type=3",
		bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("AuthorizationType", "ilink_bot_token")
	h.Set("iLink-App-ClientVersion", fmt.Sprintf("%d", ILinkAppClientVersion))
	h.Set("X-WECHAT-UIN", randomWechatUin())
	req.Header = h

	resp, err := noProxyClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var r QRCodeResp
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("qrcode parse: %w raw=%s", err, string(raw))
	}
	return &r, nil
}

// PollQRStatus 轮询扫码状态，confirmed 时返回凭证
func PollQRStatus(ctx context.Context, baseURL, qrcode string) (*QRStatusResp, error) {
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		body := map[string]any{}
		bodyBytes, _ := json.Marshal(body)
		req, err := http.NewRequestWithContext(ctx, "POST",
			baseURL+"/ilink/bot/get_qrcode_status?qrcode="+qrcode,
			bytes.NewReader(bodyBytes))
		if err != nil {
			return nil, err
		}
		h := http.Header{}
		h.Set("Content-Type", "application/json")
		h.Set("AuthorizationType", "ilink_bot_token")
		h.Set("iLink-App-ClientVersion", fmt.Sprintf("%d", ILinkAppClientVersion))
		h.Set("X-WECHAT-UIN", randomWechatUin())
		req.Header = h

		resp, err := noProxyClient.Do(req)
		if err != nil {
			return nil, err
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		var r QRStatusResp
		if err := json.Unmarshal(raw, &r); err != nil {
			return nil, fmt.Errorf("status parse: %w", err)
		}

		switch r.Status {
		case "confirmed":
			return &r, nil
		case "expired", "0":
			return nil, fmt.Errorf("二维码过期或失败: %s", r.Status)
		case "wait", "scaned", "scaned_but_redirect":
			// 继续轮询
			time.Sleep(2 * time.Second)
		default:
			time.Sleep(2 * time.Second)
		}
	}
}
