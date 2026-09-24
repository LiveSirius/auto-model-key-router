package tui

// 本文件移植 tui.py:144-231 的「终端窗体」版式算法。
//
// 这是本包最核心的共享逻辑：把若干节点渲染进一个与终端等大的圆角面板，
// 正文可滚动、底部固定快捷键提示，并在副标题上显示当前行窗口
// （tui.py:200-201）。config_editor.py / dashboard.py / logs_tui.py 都直接用
// terminal_frame_state 的返回值（renderable + offset + max_offset + viewport_height）
// 自己驱动滚动循环。

import (
	"fmt"
	"strings"
)

// FrameState 对应 tui.py:62-67 的 TerminalFrameState。
type FrameState struct {
	// Renderable 是可直接打印的窗体节点。
	Renderable Renderable
	// Offset 是正文视口当前起始行。
	Offset int
	// MaxOffset 是正文最大可滚动偏移。
	MaxOffset int
	// ViewportHeight 是正文视口高度（行）。
	ViewportHeight int
}

// FrameOptions 对应 terminal_frame_state 的关键字参数（tui.py:144-152）。
type FrameOptions struct {
	// Offset 是正文起始行。
	Offset int
	// FocusText 非空时，窗体滚动到包含该文本的第一行（tui.py:182-191）。
	FocusText string
	// PreserveBottom 为真时正文默认停在底部（tui.py:180）。
	PreserveBottom bool
	// FrameTitle 是窗体标题，空串表示用 WindowTitle。
	FrameTitle string
}

// TerminalFrameState 是 tui.py:144-212 的逐行移植。
//
// 返回值里的 Offset/MaxOffset/ViewportHeight 由调用方保存，用于下一次滚动。
// 高度不足 3 行时 rich 会退化成「直接输出正文、不套面板」（tui.py:155-158），
// Go 侧同一分支处理，由 TestFrameDegenerateHeightHasNoPanel 的 height_1/height_2
// 两个子用例钉住。
func TerminalFrameState(renderables []Renderable, footer Renderable, options FrameOptions) FrameState {
	width := maxInt(Console.Width(), MinRenderWidth)
	height := maxInt(Console.Height(), 1)
	title := options.FrameTitle
	if title == "" {
		title = WindowTitle
	}
	body := Group{Items: renderables}

	if height < 3 {
		var lines []string
		if len(renderables) > 0 {
			lines = RenderableLineSegments(body, width)
		}
		visible := FitTerminalLines(lines, height, options.PreserveBottom)
		return FrameState{
			Renderable:     SegmentLinesRenderable(visible),
			Offset:         0,
			MaxOffset:      maxInt(len(lines)-height, 0),
			ViewportHeight: height,
		}
	}

	contentWidth := maxInt(width-4, 1)
	innerHeight := height - 2
	var footerLines []string
	if footer != nil {
		footerLines = RenderableLineSegments(footer, contentWidth)
	}
	if len(footerLines) >= innerHeight {
		visibleFooter := footerLines[len(footerLines)-innerHeight:]
		panel := Panel{
			Content:  SegmentLinesRenderable(visibleFooter),
			Title:    "[bold bright_cyan]" + title + "[/bold bright_cyan]",
			Width:    width,
			Height:   height,
			Subtitle: "",
		}
		return FrameState{Renderable: panel, Offset: 0, MaxOffset: 0, ViewportHeight: 0}
	}

	separatorHeight := 0
	if len(footerLines) > 0 {
		separatorHeight = 1
	}
	viewportHeight := maxInt(innerHeight-len(footerLines)-separatorHeight, 0)
	var bodyLines []string
	if len(renderables) > 0 {
		bodyLines = RenderableLineSegments(body, contentWidth)
	}
	maxOffset := maxInt(len(bodyLines)-viewportHeight, 0)
	offset := minInt(maxInt(options.Offset, 0), maxOffset)
	if options.PreserveBottom {
		offset = maxOffset
	}

	if options.FocusText != "" && viewportHeight > 0 {
		if focusLine := findFocusLine(bodyLines, options.FocusText); focusLine >= 0 {
			if focusLine < offset {
				offset = focusLine
			} else if focusLine >= offset+viewportHeight {
				offset = minInt(focusLine-viewportHeight+1, maxOffset)
			}
		}
	}

	end := minInt(offset+viewportHeight, len(bodyLines))
	visibleBody := FitTerminalLines(sliceLines(bodyLines, offset, end), viewportHeight, false)
	windowLines := make([]string, 0, len(visibleBody)+len(footerLines)+1)
	windowLines = append(windowLines, visibleBody...)
	if len(footerLines) > 0 {
		windowLines = append(windowLines, strings.Repeat("─", contentWidth))
		windowLines = append(windowLines, footerLines...)
	}
	subtitle := ""
	if maxOffset > 0 {
		// 注意两侧各有一个空格（tui.py:201 的文案就是 " 第 ... 滚动 "），
		// 面板再各补一个空格，最终与 rich 的间距一致。
		subtitle = fmt.Sprintf("[dim] 第 %d-%d 行 / 共 %d 行 · PgUp/PgDn 滚动 [/dim]",
			offset+1, end, len(bodyLines))
	}
	panel := Panel{
		Content:  SegmentLinesRenderable(windowLines),
		Title:    "[bold bright_cyan]" + title + "[/bold bright_cyan]",
		Subtitle: subtitle,
		Width:    width,
		Height:   height,
	}
	return FrameState{Renderable: panel, Offset: offset, MaxOffset: maxOffset, ViewportHeight: viewportHeight}
}

// TerminalFrame 只取窗体的渲染节点（tui.py:215-231）。
func TerminalFrame(renderables []Renderable, footer Renderable, preserveBottom bool, options FrameOptions) Renderable {
	options.PreserveBottom = preserveBottom
	return TerminalFrameState(renderables, footer, options).Renderable
}

// findFocusLine 找出第一个包含 focusText 的正文行（tui.py:183-186）。
//
// Python 用 `focus_text in "".join(segment.text for segment in line)`，即纯子串
// 匹配；Go 侧按行文本匹配，语义相同。
func findFocusLine(lines []string, focusText string) int {
	for index, line := range lines {
		if strings.Contains(line, focusText) {
			return index
		}
	}
	return -1
}

// sliceLines 取 [start:end) 的安全切片。
func sliceLines(lines []string, start, end int) []string {
	if start < 0 {
		start = 0
	}
	if end > len(lines) {
		end = len(lines)
	}
	if start >= end {
		return nil
	}
	return lines[start:end]
}
