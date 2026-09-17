// Package tui 移植 auto_model_key_router/tui.py：共享的终端渲染助手 + 交互式界面。
//
// 为什么这个包重要：service.py、main.py、config_editor.py（以及 dashboard.py、
// logs_tui.py、update.py）都从 tui.py 导入符号，它是这些模块的公共门面。
// 因此本包分两层：
//
//  1. **共享渲染助手**（console.go / layout.go / frame.go / scroll.go / terminal.go）：
//     section_panel、page_title、menu_table、shortcut_text、terminal_frame_state、
//     content_scroll_offset、should_handle_wheel、read_key、mouse_wheel_mode、
//     open_config_file……与 Python 同名同义，供其他模块直接调用。
//  2. **交互式界面**（models.go / options.go / program.go）：select_option、
//     select_multiple、prompt_text、confirm_choice、show_result_page、run_submodule，
//     状态机照抄 Python，渲染复用第 1 层，事件循环用 Bubble Tea。
//
// # Python → Go 符号映射（供后续移植直接照抄）
//
//	console                     → Console（*Terminal，包级单例）
//	console.size.width/height   → Console.Width() / Console.Height()
//	console.print(x)            → Console.Print(x)
//	console.clear()             → Console.Clear()
//	ResultPage                  → ResultPage（CopyResultPage 构造）
//	TerminalFrameState          → FrameState
//	app_flag_title              → AppFlagTitle
//	page_title                  → PageTitle
//	menu_table                  → MenuTable
//	checkbox_menu_table         → CheckboxMenuTable
//	section_panel               → SectionPanel
//	shortcut_text               → ShortcutText
//	content_viewport_height     → ContentViewportHeight
//	renderable_line_segments    → RenderableLineSegments
//	segment_lines_renderable    → SegmentLinesRenderable
//	folded_marker_line          → FoldedMarkerLine
//	fit_terminal_lines          → FitTerminalLines
//	terminal_frame_state        → TerminalFrameState
//	terminal_frame              → TerminalFrame
//	scrollable_content_state    → ScrollableContentState
//	render_option_menu_state    → RenderOptionMenuState
//	render_option_menu          → RenderOptionMenu
//	render_multi_select_state   → RenderMultiSelectState
//	parse_sgr_mouse_sequence    → ParseSGRMouseSequence
//	should_handle_wheel         → ShouldHandleWheel / ShouldHandleWheelNow
//	content_scroll_offset       → ContentScrollOffset
//	read_windows_until          → ReadWindowsUntil
//	read_windows_char_if_available → ReadWindowsCharIfAvailable
//	read_posix_until            → ReadPosixUntil
//	read_posix_char_if_available → ReadPosixCharIfAvailable
//	read_posix_chars_if_available → ReadPosixCharsIfAvailable
//	enable_windows_virtual_terminal_input → EnableWindowsVirtualTerminalInput
//	mouse_wheel_mode            → MouseWheelMode（contextmanager → 返回恢复函数）
//	posix_input_mode            → PosixInputMode（同上）
//	key_pressed                 → KeyPressed（返回 (按键名, 是否有输入)）
//	read_key_responsive         → ReadKeyResponsive
//	read_key                    → ReadKey
//	read_posix_key              → ReadKeyFromByteReader（注入式，便于对拍）
//	confirm_choice              → ConfirmChoice
//	prompt_text                 → PromptText
//	select_option               → SelectOption
//	select_multiple             → SelectMultiple
//	clear_terminal_history      → ClearTerminalHistory
//	run_submodule               → RunSubmodule（KeyboardInterrupt → ErrCancelled）
//	show_result_page            → ShowResultPage
//	open_config_file            → OpenConfigFile
//
// 模块级常量与词表：APP_ASCII_FLAG→AppASCIIFlag、MOUSE_MODE_ENABLE/DISABLE、
// WHEEL_EVENT_INTERVAL_SECONDS→WheelEventIntervalSeconds、
// ESC_SEQUENCE_TIMEOUT_SECONDS→EscSequenceTimeoutSeconds、
// WHEEL_CONTENT_STEP→WheelContentStep、WHEEL_KEYS→WheelKeys、
// MIN_RENDER_WIDTH→MinRenderWidth、FOLDED_CONTENT_MARKER→FoldedContentMarker、
// WINDOW_TITLE→WindowTitle、WINDOW_BORDER_STYLE→WindowBorderStyle、
// SELECTED_ROW_MARKER→SelectedRowMarker。
//
// # 与 Python 的差异（全部有意为之，且各自有具名测试）
//
//  1. **样式与颜色不迁移**。迁移方案已把「富文本降级为等价纯文本」定为非目标；
//     Renderable 只产出纯文本行，`BorderStyle`/`AppASCIIFlagStyles` 只作记录。
//     交互界面的选中行用 lipgloss 高亮（非终端下自动降级为纯文本）。
//  2. **表格版式**。menu_table / checkbox_menu_table 用固定列宽，不复刻 rich 的
//     expand 列宽分配（信息等价、间距不同），见 divergence_test.go。
//  3. **标记解析不抛异常**。rich 对多余的闭合标签抛 MarkupError，StripMarkup 尽力
//     解析并返回文本。
//  4. **终端尺寸探测**。rich 会 ioctl 探测，Go 只读 COLUMNS/LINES 再退化成 80x25；
//     交互模型在 tea.WindowSizeMsg 里用 SetSize 修正。
//  5. **异常 → 错误值**。KeyboardInterrupt 用 ErrCancelled 表达；
//     run_submodule 的错误文案用 Go 类型名 + 错误文本代替「类名: 文本」。
//  6. **输入框多光标移动**。编辑状态由 bubbles/textinput 维护（Python 只有尾部追加），
//     但可见值的截断/掩码严格照抄 Python（VisiblePromptValue，语料锁定）。
//  7. **宽度表**。内置常用 East Asian Width 区段，不做 ZWJ 组合序列折叠；
//     语料只覆盖 ASCII/CJK/单个 emoji。
//
// # 对拍
//
// scripts/gen_tui_corpus.py 用**真实 Python**（含真实 rich）产出
// testdata/tui_corpus.json：把 tui.console 换成固定尺寸的受控 Console，把
// msvcrt/os.read/select/time.monotonic 换成脚本化桩，因此版式、折行、按键解析
// 这些纯逻辑都能确定性重放（corpus_test.go）。语料用 --check 校验是否过期。
package tui
