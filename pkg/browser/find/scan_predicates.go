package find

// scanJSPredicates 是扫描脚本的前半段：入口归一化（含 __WANTS__/__SELECTOR__ 等占位符）、
// 可见性与可编辑判定（__IS_EDITABLE__ 注入点）、语义打分函数 scoreEl 与几何/容器工具。
// 与 scan.go 的 scanJSTail 拼接后即原单段模板，**拼接结果逐字节等值**（拆两文件是硬约束：
// 单文件会新增代码软限 WARN，口径同 `window` 子包 3a 的 shared/anchor 拆分）。
const scanJSPredicates = `
	  const rawWants = __WANTS__;
	  const sel = __SELECTOR__;
	  const preferLast = __PREFER_LAST__;
	  const strictTarget = __STRICT__;
	  const editableOnly = __EDITABLE_ONLY__;
	  const norm = s => (s == null ? '' : String(s)).replace(/\s+/g, ' ').trim();
	  // 归一化后的候选文案集合（semantic_aliases.go / spec/algorithm-kernel.md §5.0.2）：
	  // 末位元素是原始语义标签，未覆盖的标签即单候选 ⇒ 行为零变化。
	  // 必须在 norm 定义之后求值（const 声明有 TDZ，提前调用会抛 ReferenceError）。
	  const wants = rawWants.map(v => norm(v).toLowerCase()).filter(v => v);
	  // 黑名单标签过滤（§5.0.6 条款 1）：style/script/noscript/template 不参与文本与定位。
	  const isBlacklistTag = el => {
	    if (!el || !el.tagName) return false;
	    const tag = el.tagName.toLowerCase();
	    return tag === 'style' || tag === 'script' || tag === 'noscript' || tag === 'template';
	  };
	  // 后台常用纯文本叶子承载点击；没有 ARIA/onclick 时，pointer 光标是可见的点击意图。
	  const isPointerTextLeaf = el => {
	    if (el.children.length !== 0 || isBlacklistTag(el)) return false;
	    const text = norm(el.textContent);
	    return text.length > 0 && text.length <= 120 && getComputedStyle(el).cursor === 'pointer';
	  };
	  // 几何有效性前置硬剪枝（§5.0.5 条款 1）：在打分前彻底剔除不可见、无渲染、折叠节点。
	  const physicalVisible = el => {
	    if (!el || isBlacklistTag(el)) return false;
	    if (el.getAttribute && el.getAttribute('aria-hidden') === 'true') return false;
	    const cs = getComputedStyle(el);
	    if (cs.visibility === 'hidden' || cs.display === 'none' || cs.opacity === '0') return false;
	    if (typeof el.getClientRects === 'function' && el.getClientRects().length === 0) return false;
	    const r = el.getBoundingClientRect();
	    return r.width >= 2 && r.height >= 2;
	  };
	  const visible = physicalVisible;
	  // 可编辑元素判定（§5.0.3 条款 1）：与 input_gate.go 的 input.IsEditableFnJS 同源注入，
	  // 保证定位层与派发层用的是同一份定义。
	  __IS_EDITABLE__
	  // 语义分：跨全部文本源 × 全部候选取最高分（0/60/80/100），并回带命中的候选。
	  // 比较按小写归一（页面文案大小写不可控）；旧行为「内部标签直接字面比对」
	  // 在真实站点上必然 0 命中（12 站实采，案卷 site-coverage-2026-09-24 §6）。
	  const scoreEl = el => {
	    if (isBlacklistTag(el)) return { score: 0, matched: "" };
	    let sc = 0, matched = "";
	    const bump = (v, w) => { if (v > sc) { sc = v; matched = w; } };
	    const normVal = el.value ? norm(el.value) : '';
	    // 布局感知文本提取（§5.0.6 条款 2）：textarea 作为模板容器包裹 <style> 时，忽略其 value
	    const cleanVal = (el.tagName && el.tagName.toLowerCase() === 'textarea' && /^<style/i.test(normVal)) ? '' : normVal;
	    for (const src of [el.innerText, cleanVal, el.getAttribute('aria-label'),
	                       el.getAttribute('data-testid'), el.getAttribute('title'),
	                       el.placeholder]) {
	      const s = norm(src);
	      if (!s) continue;
	      const sl = s.toLowerCase();
	      for (const w of wants) {
	        if (sl === w) bump(100, w);
	        else if (sl.indexOf(w) >= 0) bump(80, w);
	        // 60 分档（目标包含元素文本）只服务点击/输入的「宽泛语义」场景；
	        // 抽取必须拿回目标本身的文本，碎片命中的是别的节点（假阳性根因）。
	        // 2026-09-24（§5.0.2 条款 3）：候选集合扩大后碎片档假阳性面等比放大，
	        // 故要求元素文本 ≥ 2 字符，禁掉单字符碎片（「×」「»」）。
	        else if (!strictTarget && sl.length >= 2 && w.indexOf(sl) >= 0) bump(60, w);
	      }
	    }
	    // 重复短标签（如多行「修改」）要用其行上下文消歧。只给 pointer 文本叶子
	    // 读取最近行边界的限长文本；查询“登录密码”时可定位该行的「修改」，不读整页文本。
	    // 语义抽取不参与该消歧：行上下文里的词不是被抽取对象的文本。
	    if (!strictTarget && isPointerTextLeaf(el)) {
	      let parent = el.parentElement;
	      for (let depth = 0; parent && depth < 4; depth++, parent = parent.parentElement) {
	        if (parent.matches('body, html')) break;
	        // 只在最近的行边界消歧，不能让外层列表或整页文本污染候选。
	        if (!parent.matches('tr, li, [role="row"], .row, .item')) continue;
	        const context = norm(parent.innerText);
	        if (context.length <= 240) {
	          const cl = context.toLowerCase();
	          for (const w of wants) {
	            if (cl === w) bump(100, w);
	            else if (cl.indexOf(w) >= 0) bump(80, w);
	          }
	        }
	        break;
	      }
	    }
	    return { score: sc, matched: matched };
	  };
	  const areaEl = el => { const r = el.getBoundingClientRect(); return r.width * r.height; };
	  // 真正可交互（排除裸 data-testid：那只是测试锚点，不是交互能力）。
	  const isRealInteractive = el =>
	    el.matches('button, a[href], input:not([type=hidden]), textarea, select, [role="button"], [role="link"], [role="menuitem"], [onclick]')
	    || el.onclick != null || el.tabIndex >= 0 || isPointerTextLeaf(el);
	  // 结构性容器：无交互属性、只承载/聚合子元素文本 → 命中它=点中容器而非目标。
	  const isContainerLike = el => {
	    const t = el.tagName.toLowerCase();
	    // label 承载「产品描述」等表单标签文本，本身不可交互，视为容器以便下钻到
	    // 其关联输入框（2026-08-31：Airwallex 描述 textarea 无 placeholder/aria-label）。
	    return ['div','section','ul','ol','li','main','article','aside','nav','header','footer','table','dl','form','fieldset','details','label'].includes(t);
	  };
	  // 从容器下钻到最内层「可见 + 语义分>0 + 面积最小」的可交互候选（叶子）。
	  const leafOf = container => {
	    let leaf = null, leafArea = Infinity;
	    // 2026-08-31 label 关联输入框（Airwallex 描述 textarea 无 placeholder/aria-label）：
	    // label 命中但语义评分抓不到其关联控件时，按 label 的 control/htmlFor/同容器
	    // 输入框直接下钻，避免 input 语义失败后交给 VLM 坐标（多输入框表单 VLM 会报错框）。
	    if (container.matches('label') || container.querySelector('label')) {
	      const label = container.matches('label') ? container : container.querySelector('label');
	      const ctrl = label.control || (label.htmlFor ? document.getElementById(label.htmlFor) : null);
	      if (ctrl && visible(ctrl) && ['INPUT','TEXTAREA','SELECT'].includes(ctrl.tagName) && ctrl.type !== 'hidden') {
	        return ctrl;
	      }
	    }
	    for (const c of Array.from(container.querySelectorAll(
	      'button, a[href], input:not([type=hidden]), textarea, select, [role="button"], [role="link"], [role="menuitem"], [onclick], [data-testid]'))) {
	      if (!visible(c)) continue;
	      if (scoreEl(c).score <= 0) continue;
	      const a = areaEl(c);
	      if (a < leafArea) { leafArea = a; leaf = c; }
	    }
	    return leaf;
	  };`
