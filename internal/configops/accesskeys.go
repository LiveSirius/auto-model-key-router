package configops

import (
	"crypto/rand"
	"encoding/hex"
	"strings"

	"github.com/Sparrived/auto-model-key-router/internal/canonical"
	"github.com/Sparrived/auto-model-key-router/internal/config"
)

// 本文件是**访问密钥**的增删改查：分发给外部使用者的受限推理凭据。
//
// 它取代了原先的固定访客 key（`amkr-visitor`）。那把 key 的权限是「所有 allow_visitor
// 为真的上游 key」的并集——全局共享、无法按人收窄，也就无法回答「这把 key 是谁的、
// 它本该能调什么」。访问密钥把权限变成每把 key 自己的两份清单（providers 与 models），
// 于是分发、收窄、停用、轮换都成为针对单把 key 的操作。
//
// 与工作空间的两把 key 不同，访问密钥**不绑定工作空间**：它跟着请求里的
// X-AMKR-Workspace 走，与本地主凭据的用法一致。清单已经表达了要限制的东西（能碰哪些
// 上游、能调哪些模型），再绑一个空间只会让「发给外部试用者」这件事多一道没必要的配置。

// CreatedAccessKey 是新建访问密钥的结果：稳定 ID 与**明文** key。
//
// 明文只在这个返回值里出现一次。之后任何 GET 都只回指纹——凭据散落到列表响应里，
// 就等于每次打开管理页都重新泄漏一遍。
type CreatedAccessKey struct {
	// ID 是配置里的键（`access_keys.<id>`），更新、轮换、删除都用它定位。
	ID string
	// Name 是给人看的标识。
	Name string
	// Key 是密钥明文。
	Key string
}

// SetList 表示「本次是否要改这个清单」以及改成什么。
//
// 三态是必要的，不能折叠成 `[]string`：
//   - Present 为假：调用方没提这个字段，保持原样；
//   - Present 为真且 Values 为 nil：清除限制（删掉字段，回到不限制）；
//   - Present 为真且 Values 非 nil（可以是空切片）：显式写入这份清单，空切片即
//     「一个都不许」。
//
// 这正是 config.AccessKeyConfig 里 nil 与空切片语义不同的来源，必须在写盘前就保住。
type SetList struct {
	Present bool
	Values  []string
}

// CreateAccessKeyOptions 是 CreateAccessKey 的可选参数。
//
// Key 为空表示由服务端生成（WebUI 走这条）；外部系统传入自己的 key 时原样采用——
// 调用方可能已经有既定的凭据命名规范，替它改名只会让对接方多一层映射。
type CreateAccessKeyOptions struct {
	Key       string
	Providers SetList
	Models    SetList
	Enabled   *bool
}

// CreateAccessKey 新建一把访问密钥并返回它的明文。
//
// id 由服务端生成（32 位十六进制，与 uuid4().hex 同形）：它是配置里的稳定定位符，
// 让调用方自己取 id 只会引入「重名怎么办」这种没有意义的问题——名字是给人的标签，
// 允许重复，也允许随时改。
func CreateAccessKey(data *canonical.Value, name string, options CreateAccessKeyOptions) (CreatedAccessKey, error) {
	trimmed, err := nonEmptyString(name, "访问密钥名称")
	if err != nil {
		return CreatedAccessKey{}, err
	}
	id, err := generateAccessKeyID()
	if err != nil {
		return CreatedAccessKey{}, err
	}
	keys := accessKeysObject(data)

	secret := strings.TrimSpace(options.Key)
	if secret == "" {
		if secret, err = config.GenerateAccessKey(); err != nil {
			return CreatedAccessKey{}, err
		}
	}
	// 与 config.Validate 同一组底线，理由见那里的说明。这里判是为了让请求当场拿到
	// 4xx，而不是写进一个下次热重载才失败的配置（_update_config 不跑 FromDict）。
	if err := checkAccessKeySecret(data, "key", secret, id); err != nil {
		return CreatedAccessKey{}, err
	}

	entry := canonical.NewObject()
	entry.SetKey("name", canonical.NewString(trimmed))
	entry.SetKey("key", canonical.NewString(secret))
	enabled := true
	if options.Enabled != nil {
		enabled = *options.Enabled
	}
	entry.SetKey("enabled", canonical.NewBool(enabled))
	if err := writeAccessKeyScope(data, entry, id, options.Providers, options.Models); err != nil {
		return CreatedAccessKey{}, err
	}
	keys.SetKey(id, entry)
	data.SetKey("access_keys", keys)
	return CreatedAccessKey{ID: id, Name: trimmed, Key: secret}, nil
}

// UpdateAccessKeyOptions 是 UpdateAccessKey 的可选参数。
//
// nil 指针一律表示「本次不改这个字段」。Providers / Models 用 SetList 表达三态。
type UpdateAccessKeyOptions struct {
	Name      *string
	Enabled   *bool
	Providers SetList
	Models    SetList
}

// UpdateAccessKey 修改一把访问密钥的名字、启停与两份清单。
//
// 不在这里换 key：轮换是独立操作（RotateAccessKey），因为它有独立的后果——旧 key
// 立刻失效，调用方必须把新的交给使用者。把两件事塞进一个「更新」里，会让一次改名
// 顺手把别人正在用的凭据换掉。
func UpdateAccessKey(data *canonical.Value, id string, options UpdateAccessKeyOptions) error {
	entry, err := requireAccessKey(data, id)
	if err != nil {
		return err
	}
	if options.Name != nil {
		name, err := nonEmptyString(*options.Name, "访问密钥名称")
		if err != nil {
			return err
		}
		entry.SetKey("name", canonical.NewString(name))
	}
	if options.Enabled != nil {
		entry.SetKey("enabled", canonical.NewBool(*options.Enabled))
	}
	return writeAccessKeyScope(data, entry, id, options.Providers, options.Models)
}

// RotateAccessKey 换掉一把访问密钥的明文并返回新值。
//
// 旧 key 随字段覆盖立即失效（配置里只存一个值）。这正是轮换的意义：泄漏的那把在
// 写盘的那一刻就不再被任何判定命中。
func RotateAccessKey(data *canonical.Value, id string) (string, error) {
	entry, err := requireAccessKey(data, id)
	if err != nil {
		return "", err
	}
	secret, err := config.GenerateAccessKey()
	if err != nil {
		return "", err
	}
	if err := checkAccessKeySecret(data, "key", secret, id); err != nil {
		return "", err
	}
	entry.SetKey("key", canonical.NewString(secret))
	return secret, nil
}

// DeleteAccessKey 删除一把访问密钥；它是最后一把时连 access_keys 段一起清掉。
//
// 与 deleteWorkspace 同一策略：空段留在配置里没有意义，还会让「有没有配过访问密钥」
// 在配置文件里看起来是真。删除后那把 key 立即失效。
func DeleteAccessKey(data *canonical.Value, id string) error {
	keys := lookup(data, "access_keys")
	if !keys.IsObject() {
		return opErrf(404, "访问密钥不存在: %s", id)
	}
	if _, err := requireAccessKey(data, id); err != nil {
		return err
	}
	keys.DeleteKey(id)
	if keys.Obj.Len() == 0 {
		data.DeleteKey("access_keys")
		return nil
	}
	data.SetKey("access_keys", keys)
	return nil
}

// AccessKeyExists 报告配置里是否已有该 id 的访问密钥。
func AccessKeyExists(data *canonical.Value, id string) bool {
	return lookup(lookup(data, "access_keys"), strings.TrimSpace(id)).IsObject()
}

// requireAccessKey 返回指定访问密钥，不存在时报 404。
func requireAccessKey(data *canonical.Value, id string) (*canonical.Value, error) {
	trimmed := strings.TrimSpace(id)
	if trimmed == "" {
		return nil, opErr(404, "访问密钥不存在: ")
	}
	entry := lookup(lookup(data, "access_keys"), trimmed)
	if !entry.IsObject() {
		return nil, opErrf(404, "访问密钥不存在: %s", trimmed)
	}
	return entry, nil
}

// accessKeysObject 返回 data["access_keys"]，缺失时先写入一个空对象。
func accessKeysObject(data *canonical.Value) *canonical.Value {
	keys := lookup(data, "access_keys")
	if !keys.IsObject() {
		keys = canonical.NewObject()
		data.SetKey("access_keys", keys)
	}
	return keys
}

// writeAccessKeyScope 按三态语义写入 providers / models 两份清单。
//
// 逐个校验引用的目标确实存在（供应商 ID、模型名或别名）。写错一个名字会让某把已经
// 分发出去的 key 静默少一项权限——调用方那边只看到 403，而排查要同时翻配置与调用方
// 两侧。错误文本与 config.parseAccessKeyProviders / parseAccessKeyModels 逐字一致，
// 因为那是最终防线对同一份数据的同一句判定。
func writeAccessKeyScope(
	data *canonical.Value,
	entry *canonical.Value,
	id string,
	providers SetList,
	models SetList,
) error {
	if providers.Present {
		if providers.Values == nil {
			entry.DeleteKey("providers")
		} else {
			known := configuredProviderIDs(data)
			list := canonical.NewArray()
			for index, raw := range providers.Values {
				providerID := strings.TrimSpace(raw)
				if providerID == "" {
					return opErrf(422, "access_keys.%s.providers[%d] 不能为空", id, index)
				}
				if !known[providerID] {
					return opErrf(422, "access_keys.%s.providers[%d] 引用了未配置的供应商: %s", id, index, providerID)
				}
				list.Arr = append(list.Arr, canonical.NewString(providerID))
			}
			entry.SetKey("providers", list)
		}
	}
	if models.Present {
		if models.Values == nil {
			entry.DeleteKey("models")
		} else {
			known := configuredModelNames(data)
			list := canonical.NewArray()
			for index, raw := range models.Values {
				modelName := strings.TrimSpace(raw)
				if modelName == "" {
					return opErrf(422, "access_keys.%s.models[%d] 不能为空", id, index)
				}
				if !known[modelName] {
					return opErrf(422, "access_keys.%s.models[%d] 引用了未配置的模型: %s", id, index, modelName)
				}
				list.Arr = append(list.Arr, canonical.NewString(modelName))
			}
			entry.SetKey("models", list)
		}
	}
	return nil
}

// checkAccessKeySecret 判定一把待写入的访问密钥明文是否可用，不可用时返回 422。
//
// 与 checkWorkspaceSecret 是同一组底线，且**互相视为占用**：本地主凭据、工作空间两把
// key、其它访问密钥都不允许在实例内重复出现。同一个字符串在两处都命中会让它的权限
// 取决于先命中哪张清单——那是把权限边界交给判定顺序，不是配置能表达的东西。
//
// exceptID 是本次写入的位置，与它自己重复不算冲突（轮换时旧值还躺在配置里）。
func checkAccessKeySecret(data *canonical.Value, kind, secret, exceptID string) error {
	if secret == "" {
		return nil
	}
	if local := strings.TrimSpace(lookup(data, "local_api_key").StringValue()); local != "" && secret == local {
		return opErrf(422, "访问密钥的 %s 不能与 local_api_key 相同", kind)
	}
	if owner, isAccessKey := secretOwnerOutside(data, secret, "", exceptID); owner != "" {
		// 占用者是工作空间时是裸空间名，需要补上「的 key」；是访问密钥时 owner 已是
		// 自足描述（`访问密钥 t 的 key`），再补会拼出「访问密钥 访问密钥 t 的 key」。
		if isAccessKey {
			return opErrf(422, "访问密钥 %s 的 %s 与%s重复", exceptID, kind, owner)
		}
		return opErrf(422, "访问密钥 %s 的 %s 与工作空间 %s 的 key 重复", exceptID, kind, owner)
	}
	return nil
}

// configuredProviderIDs 返回配置里全部供应商 ID。
func configuredProviderIDs(data *canonical.Value) map[string]bool {
	ids := map[string]bool{}
	for _, pair := range objectItems(lookup(data, "providers")) {
		ids[pair.Key] = true
	}
	return ids
}

// generateAccessKeyID 生成访问密钥的稳定 ID：16 字节的十六进制（32 字符）。
//
// 与 `uuid.uuid4().hex` 同形，便于把管理面既有的 id 处理（探测 id、备份名后缀）
// 直接复用；这里不复刻 UUID 的版本位，因为没有任何东西会去解析它——它只是一个
// 足够随机、不会撞的字符串键。
func generateAccessKeyID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
