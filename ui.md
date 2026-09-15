STYLEKIT_STYLE_REFERENCE
style_name: 材料设计
style_slug: material-design
style_source: /styles/material-design

# Hard Prompt

## 什么时候用
当你希望 AI 严格按风格规则生成代码时使用。它是生产界面最稳的默认选择。

## 怎么用
- 把完整提示词复制到 ChatGPT、Claude、Cursor 或其他编码助手。
- 在提示词后追加具体产品、页面或组件需求。
- 生成后按禁止项和交互状态检查，确认没有风格漂移。

请严格遵守以下风格规则并保持一致性，禁止风格漂移。

## 执行要求

- 优先保证风格一致性，其次再做创意延展。
- 遇到冲突时以禁止项为最高优先级。
- 输出前自检：颜色、排版、间距、交互是否仍属于该风格。

## Style Rules

你是一个 Material Design 设计风格的前端开发专家。生成的所有代码必须严格遵守以下约束：

## 绝对禁止

- 禁止使用不一致的阴影深度
- 禁止使用过于柔和的配色
- 禁止省略交互反馈
- 禁止打破 8dp 网格系统
- 禁止按钮缺少 active:scale-[0.98]（Material Pseudo-Ripple 是触感真实性的核心）
- 禁止使用非 Material 标准缓动曲线（必须使用 cubic-bezier(0.4,0,0.2,1)）
- 禁止卡片 hover 时阴影不变深（海拔变化是 Material 物理规则，不可省略）

## 必须遵守

- 使用海拔阴影系统表达层次
- 应用涟漪效果作为点击反馈
- 使用大胆鲜明的色彩
- 保持 8dp 的间距网格
- 使用 Roboto 字体
- 添加有意义的微动效
- 精确双层海拔阴影：hover 时从 dp2 升至 dp8，shadow-[0_1px_3px_rgba(0,0,0,0.12),0_1px_2px_rgba(0,0,0,0.24)] → shadow-[0_14px_28px_rgba(0,0,0,0.25),0_10px_10px_rgba(0,0,0,0.22)]（Elevation Physics）
- 使用 Material 标准缓动曲线 ease-[cubic-bezier(0.4,0,0.2,1)] duration-[250ms]（Deceleration Curve）
- 按钮 active:scale-[0.98]（Pseudo-Ripple，手指下压的涟漪前奏）
- 卡片 hover:-translate-y-1 配合海拔阴影升级（Z 轴物理抬升感）

## 配色

- 主色: #6200ee (紫色)
- 主色变体: #3700b3
- 次要色: #03dac6 (青色)
- 背景: #fafafa
- 表面: #ffffff
- 错误: #b00020

## 间距

- 基于 8dp 网格
- p-2 (8px), p-4 (16px), p-6 (24px), p-8 (32px)

## Animation & Interaction Rules

- Elevation Physics: hover 时从低海拔阴影抬升到高海拔阴影，可配合轻微 -translate-y-1 强化 Z 轴感。
- Pseudo-Ripple: active 状态至少包含 active:scale-[0.98] 或明暗下压反馈，模拟触控涟漪前奏。
- Deceleration Curve: 交互过渡优先使用 ease-[cubic-bezier(0.4,0,0.2,1)]，时长 200-300ms。
- Input Float: 输入框聚焦时标签必须平滑上浮并缩小，边框高亮同步过渡。

---

# Material Design (材料设计) Design System

> Google 推出的设计系统，基于纸张和墨水的隐喻，强调层次、动效、大胆色彩和响应式交互，是现代移动端设计的标准。

## 核心理念

Material Design（材料设计）是 Google 在 2014 年推出的设计语言，将数字界面比作有物理属性的纸张和墨水。

核心理念：
- 材料隐喻：界面如同有厚度的纸张，可堆叠、移动
- 海拔系统：通过阴影表达层次关系
- 大胆色彩：鲜明的主色和强调色
- 有意义的动效：动画传达空间关系和反馈

设计原则：
- 视觉一致性：所有组件必须遵循统一的视觉语言，从色彩到字体到间距保持谐调
- 层次分明：通过颜色深浅、字号大小、留白空间建立清晰的信息层级
- 交互反馈：每个可交互元素都必须有明确的 hover、active、focus 状态反馈
- 响应式适配：设计必须在移动端、平板、桌面端上保持一致的体验
- 无障碍性：确保色彩对比度符合 WCAG 2.1 AA 标准，所有交互元素可键盘访问

---

## Token 字典（精确 Class 映射）

### 边框
```
宽度: border
颜色: border-[#e0e0e0]
圆角: rounded-md
```

### 阴影
```
小: shadow-[0_1px_3px_rgba(0,0,0,0.12),0_1px_2px_rgba(0,0,0,0.14)]
中: shadow-[0_3px_6px_rgba(0,0,0,0.15),0_2px_4px_rgba(0,0,0,0.12)]
大: shadow-[0_10px_20px_rgba(0,0,0,0.15),0_3px_6px_rgba(0,0,0,0.1)]
悬停: hover:shadow-[0_14px_28px_rgba(0,0,0,0.18),0_5px_10px_rgba(0,0,0,0.12)]
聚焦: focus:shadow-[0_3px_6px_rgba(0,0,0,0.15),0_2px_4px_rgba(0,0,0,0.12)]
```

### 交互效果
```
悬停位移: （无）
悬停缩放: hover:scale-[1.01]
悬停透明度: （无）
过渡动画: transition-all duration-200 ease-in-out
按下状态: active:scale-[0.99]
```

### 字体
```
标题: font-sans font-medium tracking-tight
正文: font-sans
等宽: font-mono
```

### 字号
```
Hero: text-4xl md:text-6xl lg:text-7xl
H1: text-3xl md:text-5xl
H2: text-2xl md:text-3xl
H3: text-xl md:text-2xl
正文: text-sm md:text-base
小字: text-xs md:text-sm
```

### 间距
```
Section: py-10 md:py-16 lg:py-24
容器: px-4 md:px-6 lg:px-8
卡片: p-4 md:p-6
小间距: gap-2 md:gap-3
中间距: gap-3 md:gap-4
大间距: gap-4 md:gap-6
```

### 颜色角色
```
背景主色: bg-white
背景辅色: bg-[#f5f5f5]
背景强调色: bg-[#6200ee], bg-[#03dac6], bg-[#018786], bg-[#bb86fc]
正文主色: text-[#212121]
正文辅色: text-[#757575]
正文弱化色: text-[#9e9e9e]
按钮主色: bg-[#6200ee] text-white
按钮辅色: bg-transparent text-[#6200ee] border border-[#6200ee]
```

---

## [FORBIDDEN] 绝对禁止

以下 class 在本风格中**绝对禁止使用**，生成时必须检查并避免：

### 禁止的 Class
- `rounded-none`
- `border-black`
- `border-4`
- `shadow-[2px_2px_0px`
- `shadow-[4px_4px_0px`
- `shadow-[8px_8px_0px`
- `font-black`
- `font-serif`
- `bg-gradient-to-r`
- `bg-gradient-to-l`

### 禁止的模式
- 匹配 `^rounded-none$`
- 匹配 `^shadow-\[\d+px_\d+px_0px`
- 匹配 `^font-(?:black|serif)$`
- 匹配 `^border-(?:black|4)$`

### 禁止原因
- `rounded-none`: Material Design uses subtle rounding (rounded-md) for card elevation
- `shadow-[4px_4px_0px`: Material Design uses elevation-based blur shadows, not hard-edge
- `border-black`: Material Design uses subtle gray borders or relies on elevation shadows
- `font-serif`: Material Design uses clean sans-serif typography (Roboto-style)

> WARNING: 如果你的代码中包含以上任何 class，必须立即替换。

---

## [REQUIRED] 必须包含

### 按钮必须包含
```
rounded-md
shadow-[0_1px_3px_rgba(0,0,0,0.12),0_1px_2px_rgba(0,0,0,0.14)]
hover:shadow-[0_14px_28px_rgba(0,0,0,0.18),0_5px_10px_rgba(0,0,0,0.12)]
transition-all duration-200 ease-in-out
font-medium
```

### 卡片必须包含
```
rounded-md
shadow-[0_1px_3px_rgba(0,0,0,0.12),0_1px_2px_rgba(0,0,0,0.14)]
bg-white
```

### 输入框必须包含
```
rounded-md
border border-[#e0e0e0]
bg-white
font-sans
focus:border-[#6200ee]
focus:outline-none
```

---

## [COMPARE] Material Design 错误 vs 正确对比

以下错误示例只代表“未经过当前风格适配的通用默认值”，不要把错误示例当成视觉建议。

### 按钮

[WRONG] **错误示例**（通用组件库默认样式，不要直接复制）：
```html
<button class="{GENERIC_LIBRARY_BUTTON_DEFAULT}">
  点击我
</button>
```

[CORRECT] **正确示例**（使用当前风格的 token）：
```html
<button class="rounded-md shadow-[0_1px_3px_rgba(0,0,0,0.12),0_1px_2px_rgba(0,0,0,0.14)] hover:shadow-[0_14px_28px_rgba(0,0,0,0.18),0_5px_10px_rgba(0,0,0,0.12)] transition-all duration-200 ease-in-out font-medium bg-[#6200ee] text-white">
  点击我
</button>
```

### 卡片

[WRONG] **错误示例**（未经当前风格适配的通用卡片）：
```html
<div class="{GENERIC_LIBRARY_CARD_DEFAULT}">
  <h3>{TITLE}</h3>
</div>
```

[CORRECT] **正确示例**（使用当前风格的 card token）：
```html
<div class="rounded-md shadow-[0_1px_3px_rgba(0,0,0,0.12),0_1px_2px_rgba(0,0,0,0.14)] bg-white p-4 md:p-6">
  <h3 class="font-sans font-medium tracking-tight text-xl md:text-2xl">{TITLE}</h3>
</div>
```

### 输入框

[WRONG] **错误示例**（未经当前风格适配的通用输入框）：
```html
<input class="{GENERIC_LIBRARY_INPUT_DEFAULT}" />
```

[CORRECT] **正确示例**（使用当前风格的 input token）：
```html
<input class="rounded-md border border-[#e0e0e0] bg-white font-sans focus:border-[#6200ee] focus:outline-none" placeholder="{PLACEHOLDER}" />
```

---

## [TEMPLATES] Material Design 页面骨架模板

以下骨架只使用当前风格的 token。替换 `{PLACEHOLDER}` 时，不要移除或替换这些 token：

### 导航栏骨架
```html
<nav class="bg-white text-[#212121] border border-[#e0e0e0] px-4 md:px-6 lg:px-8">
  <div class="flex items-center justify-between max-w-6xl mx-auto gap-3 md:gap-4">
    <a href="/" class="font-sans font-medium tracking-tight text-xl md:text-2xl">
      {LOGO_TEXT}
    </a>
    <div class="flex gap-3 md:gap-4 font-sans text-xs md:text-sm">
      {NAV_LINKS}
    </div>
  </div>
</nav>
```

### Hero 区块骨架
```html
<section class="bg-[#6200ee] text-[#212121] py-10 md:py-16 lg:py-24 px-4 md:px-6 lg:px-8">
  <div class="max-w-4xl mx-auto">
    <h1 class="font-sans font-medium tracking-tight text-4xl md:text-6xl lg:text-7xl">
      {HEADLINE}
    </h1>
    <p class="font-sans text-sm md:text-base max-w-xl">
      {SUBHEADLINE}
    </p>
    <button class="rounded-md shadow-[0_1px_3px_rgba(0,0,0,0.12),0_1px_2px_rgba(0,0,0,0.14)] hover:shadow-[0_14px_28px_rgba(0,0,0,0.18),0_5px_10px_rgba(0,0,0,0.12)] transition-all duration-200 ease-in-out font-medium bg-[#6200ee] text-white">
      {CTA_TEXT}
    </button>
  </div>
</section>
```

### 卡片网格骨架
```html
<section class="bg-white text-[#212121] py-10 md:py-16 lg:py-24 px-4 md:px-6 lg:px-8">
  <div class="max-w-6xl mx-auto">
    <h2 class="font-sans font-medium tracking-tight text-2xl md:text-3xl">{SECTION_TITLE}</h2>
    <div class="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-3 gap-3 md:gap-4">
      <!-- Card template - repeat for each card -->
      <div class="rounded-md shadow-[0_1px_3px_rgba(0,0,0,0.12),0_1px_2px_rgba(0,0,0,0.14)] bg-white p-4 md:p-6">
        <h3 class="font-sans font-medium tracking-tight text-xl md:text-2xl">{CARD_TITLE}</h3>
        <p class="font-sans text-sm md:text-base text-[#9e9e9e]">{CARD_DESCRIPTION}</p>
      </div>
    </div>
  </div>
</section>
```

### 表单输入骨架
```html
<input class="rounded-md border border-[#e0e0e0] bg-white font-sans focus:border-[#6200ee] focus:outline-none" placeholder="{PLACEHOLDER}" />
```

### 页脚骨架
```html
<footer class="bg-[#f5f5f5] text-[#757575] py-10 md:py-16 lg:py-24 px-4 md:px-6 lg:px-8">
  <div class="max-w-6xl mx-auto">
    <div class="grid grid-cols-1 md:grid-cols-3 gap-4 md:gap-6">
      <div>
        <span class="font-sans font-medium tracking-tight text-xl md:text-2xl">{LOGO_TEXT}</span>
        <p class="font-sans text-xs md:text-sm">{TAGLINE}</p>
      </div>
      <div>
        <h4 class="font-sans font-medium tracking-tight text-xl md:text-2xl">{COLUMN_TITLE}</h4>
        <ul class="font-sans text-xs md:text-sm">
          {FOOTER_LINKS}
        </ul>
      </div>
    </div>
  </div>
</footer>
```

---

## [CHECKLIST] Material Design 生成后自检清单

**输出代码前，逐项验证当前风格的 token 和规则。如有违反，先修正再交付：**

### Token 检查
- [ ] 按钮包含： `rounded-md shadow-[0_1px_3px_rgba(0,0,0,0.12),0_1px_2px_rgba(0,0,0,0.14)] hover:shadow-[0_14px_28px_rgba(0,0,0,0.18),0_5px_10px_rgba(0,0,0,0.12)] transition-all duration-200 ease-in-out font-medium`
- [ ] 卡片包含： `rounded-md shadow-[0_1px_3px_rgba(0,0,0,0.12),0_1px_2px_rgba(0,0,0,0.14)] bg-white`
- [ ] 输入框包含： `rounded-md border border-[#e0e0e0] bg-white font-sans focus:border-[#6200ee] focus:outline-none`

### 禁止项检查
- [ ] 没有使用 `rounded-none`
- [ ] 没有使用 `border-black`
- [ ] 没有使用 `border-4`
- [ ] 没有使用 `shadow-[2px_2px_0px`
- [ ] 没有使用 `shadow-[4px_4px_0px`
- [ ] 没有使用 `shadow-[8px_8px_0px`
- [ ] 没有使用 `font-black`
- [ ] 没有使用 `font-serif`

### 风格规则检查
- [ ] 使用海拔阴影系统表达层次
- [ ] 应用涟漪效果作为点击反馈
- [ ] 使用大胆鲜明的色彩
- [ ] 保持 8dp 的间距网格
- [ ] 使用 Roboto 字体

### 风格漂移检查
- [ ] 没有违反：禁止使用不一致的阴影深度
- [ ] 没有违反：禁止使用过于柔和的配色
- [ ] 没有违反：禁止省略交互反馈
- [ ] 没有违反：禁止打破 8dp 网格系统
- [ ] 没有违反：禁止按钮缺少 active:scale-[0.98]（Material Pseudo-Ripple 是触感真实性的核心）

### 通用交付检查
- [ ] 响应式布局在手机、平板和桌面下稳定，没有横向溢出
- [ ] 所有交互元素有清晰焦点、可访问名称和 reduced-motion 方案
- [ ] 文本对比度达到 WCAG AA，且没有用颜色单独传递状态
- [ ] 结果仍然能够一眼识别为 Material Design

---

## [EXAMPLES] 示例 Prompt

### 1. 任务管理应用

Material 风格的任务管理界面

```
用 Material Design 创建一个任务管理应用界面，要求：
1. 顶部应用栏带阴影
2. 浮动操作按钮 (FAB)
3. 卡片列表展示任务
4. 使用海拔阴影系统
5. 紫色主色调，青色强调
```

### 2. SaaS 着陆页

生成 材料设计风格的 SaaS 产品着陆页

```
Create a SaaS landing page using Material Design style with hero section, feature grid, testimonials, pricing table, and footer.
```

### 3. 作品集展示

生成 材料设计风格的作品集页面

```
Create a portfolio showcase page using Material Design style with project grid, about section, contact form, and consistent visual language.
```

## 绝对禁止（匹配即拒绝）

以下模式一旦出现，视为风格违规——不找借口，直接重写。

- 使用不一致的阴影深度
- 使用过于柔和的配色
- 省略交互反馈
- 打破 8dp 网格系统
- 按钮缺少 active:scale-[0.98]（Material Pseudo-Ripple 是触感真实性的核心）
- 使用非 Material 标准缓动曲线（必须使用 cubic-bezier(0.4,0,0.2,1)）
- 卡片 hover 时阴影不变深（海拔变化是 Material 物理规则，不可省略）

## 自检清单（交付前逐条确认）

如果任何一条不通过，说明风格漂移了——修改后再交付。

- [ ] 没有紫色到蓝色的渐变
- [ ] 没有使用 Inter / Roboto / Geist 等过度使用的字体
- [ ] 没有嵌套卡片（卡片里面套卡片）
- [ ] 没有在彩色背景上放灰色文字
- [ ] 正文对比度满足 WCAG AA（≥4.5:1）
- [ ] 没有 bounce / elastic 缓动曲线
- [ ] 动效有 prefers-reduced-motion 备选方案
- [ ] 正文行宽不超过 65-75 个字符
- [ ] 没有单侧粗边框装饰（border-left/right accent stripe）
- [ ] 没有渐变文字（background-clip: text）
- [ ] 没有把玻璃态（glassmorphism）当作默认风格
- [ ] 没有 tiny uppercase tracked eyebrow 放在每个 section 标题上面
- [ ] 禁止使用不一致的阴影深度
- [ ] 禁止使用过于柔和的配色
- [ ] 禁止省略交互反馈
- [ ] 禁止打破 8dp 网格系统
- [ ] 禁止按钮缺少 active:scale-[0.98]（Material Pseudo-Ripple 是触感真实性的核心）