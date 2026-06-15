package openai

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
)

const (
	imagePromptGuardMinorHarm      = "minor_harm"
	imagePromptGuardMinorSexual    = "minor_sexual"
	imagePromptGuardGraphicGore    = "graphic_gore"
	imagePromptGuardSexualViolence = "sexual_violence"

	localImagePromptGuardErrorType = "cpa_local_image_prompt_guard"
	localImagePromptGuardErrorCode = "cpa_local_image_prompt_guard_violation"
)

type imagePromptGuardDecision struct {
	Block    bool
	Category string
}

func rejectUnsafeImagePrompt(c *gin.Context, prompt string) bool {
	decision := classifyImagePromptSafety(prompt)
	if !decision.Block {
		return false
	}
	c.Header("X-CPA-Local-Guard", "image_prompt")
	c.Header("X-CPA-Local-Guard-Category", decision.Category)
	c.JSON(http.StatusBadRequest, handlers.ErrorResponse{
		Error: handlers.ErrorDetail{
			Message: localImagePromptGuardMessage(decision.Category),
			Type:    localImagePromptGuardErrorType,
			Code:    localImagePromptGuardErrorCode,
		},
	})
	return true
}

func isUnsafeImagePrompt(prompt string) bool {
	return classifyImagePromptSafety(prompt).Block
}

func classifyImagePromptSafety(prompt string) imagePromptGuardDecision {
	normalized := strings.ToLower(strings.TrimSpace(prompt))
	if normalized == "" {
		return imagePromptGuardDecision{}
	}

	hasMinorContext := imagePromptContainsAny(normalized, []string{
		"未成年", "未成年人", "儿童", "小孩", "孩子", "幼童", "婴儿", "学生", "小学生", "中学生", "校园", "教室",
		"minor", "minors", "child", "children", "kid", "kids", "student", "students", "school", "classroom",
	})
	if hasMinorContext && imagePromptContainsAny(normalized, []string{
		"未成年人施暴", "对未成年人施暴", "儿童施暴", "殴打儿童", "殴打学生", "虐待儿童", "虐待学生", "攻击儿童", "攻击学生", "伤害儿童", "伤害学生",
		"violence toward minors", "violence against minors", "assaulting a child", "assaulting children", "abusing a child", "harming a child",
	}) {
		return imagePromptGuardDecision{Block: true, Category: imagePromptGuardMinorHarm}
	}
	if hasMinorContext && imagePromptContainsAny(normalized, []string{
		"裸露", "裸体", "色情", "性感", "性暗示", "性行为", "亲密姿势", "内衣", "走光",
		"nude", "nudity", "naked", "sexual", "sexually", "erotic", "porn", "underwear", "lingerie", "intimate pose",
	}) {
		return imagePromptGuardDecision{Block: true, Category: imagePromptGuardMinorSexual}
	}

	hasSafetyMitigation := imagePromptContainsAny(normalized, []string{
		"不出现", "不要出现", "不包含", "不要包含", "没有血", "无血", "没有暴力", "无暴力", "不涉及暴力", "避免血腥", "避免伤害细节",
		"without blood", "no blood", "without violence", "no violence", "non-graphic", "avoid gore", "avoid graphic", "no visible injuries",
	})
	hasDirectViolenceAction := imagePromptContainsAny(normalized, []string{
		"施暴", "暴力", "殴打", "被殴打", "打伤", "虐待", "攻击", "被击打", "assault", "abuse", "attack", "violent",
	})
	hasSpecificViolenceAction := imagePromptContainsAny(normalized, []string{
		"施暴", "殴打", "被殴打", "打伤", "虐待", "攻击", "被击打", "assault", "abuse", "attack",
	})
	if hasSafetyMitigation && hasMinorContext && (!hasDirectViolenceAction || !hasSpecificViolenceAction) {
		return imagePromptGuardDecision{}
	}
	if hasMinorContext && imagePromptContainsAny(normalized, []string{
		"施暴", "暴力", "殴打", "打伤", "伤害", "虐待", "攻击", "血腥", "流血", "出血", "伤口", "击打落点", "打击落点",
		"violence", "violent", "assault", "abuse", "harm", "injury", "injured", "attack", "gore", "bloody", "blood", "wound",
	}) {
		return imagePromptGuardDecision{Block: true, Category: imagePromptGuardMinorHarm}
	}

	if imagePromptContainsAny(normalized, []string{
		"性侵", "强奸", "猥亵", "强迫发生性关系", "sexual assault", "rape", "raping", "molest", "molestation",
	}) {
		return imagePromptGuardDecision{Block: true, Category: imagePromptGuardSexualViolence}
	}
	if imagePromptContainsAny(normalized, []string{
		"血腥", "流血", "出血", "伤口", "肢解", "断肢", "内脏", "尸体", "斩首", "碎尸",
		"gore", "bloody", "blood pooling", "dismember", "dismembered", "mutilated", "corpse", "decapitated", "severed limb", "intestines",
	}) && imagePromptContainsAny(normalized, []string{
		"写实", "真实", "逼真", "高清", "照片", "特写", "细节", "graphic", "realistic", "photorealistic", "close-up", "detailed",
	}) {
		return imagePromptGuardDecision{Block: true, Category: imagePromptGuardGraphicGore}
	}
	return imagePromptGuardDecision{}
}

func imagePromptContainsAny(text string, needles []string) bool {
	for _, needle := range needles {
		if strings.Contains(text, needle) {
			return true
		}
	}
	return false
}

func localImagePromptGuardMessage(category string) string {
	category = strings.TrimSpace(category)
	if category == "" {
		category = "unsafe_image_prompt"
	}
	return "CPA本地拦截：禁止生成此类图片。该提示词命中本地图片安全规则（" + category + "），涉嫌违反中华人民共和国相关法律法规，包括《中华人民共和国网络安全法》第十二条关于不得利用网络传播暴力、淫秽色情等信息的规定；如涉及未成年人，还可能触犯未成年人保护相关规定。请立即停止提交此类提示词，重复提交可能导致账号限制、封禁或进一步风控处理。"
}
