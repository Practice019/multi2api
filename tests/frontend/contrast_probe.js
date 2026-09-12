// contrast_probe.js —— 对比度探针（独立文件，脱离模板字符串）。
//
// # 为什么单独写
//
// 原先这段逻辑内嵌在 remaining_gaps.js 的模板字符串里，改一次就括号失衡一次，
// 而且报错信息（"missing ) after argument list"）指向的是宿主文件而非真正的
// 出错位置，排查成本很高。把它提成独立文件后：
//   - 用 node --check 就能验证语法
//   - 逻辑可被多个测试复用
//   - 不再受模板字符串转义规则的折磨
//
// # 这个探针要回答的问题
//
// 「浅色主题下，页面各组件的**实际呈现**对比度是否达到 WCAG AA（4.5:1）」。
// 难点是求"实际呈现的底色"：页面上大量元素背景是半透明的 color-mix，
// 必须把从 body 到该元素的**所有背景自上而下合成**，才能得到真实颜色。
'use strict';

// 把任意 CSS 颜色串解析成 {r,g,b,a}（r/g/b 为 0-255，a 为 0-1）。
// 必须支持 color(srgb r g b / a) —— 那是 Chrome 对 color-mix 的序列化形式。
function parseColor(s) {
  if (!s) return null;
  let m = String(s).match(/^rgba?\(([^)]+)\)$/);
  if (m) {
    const p = m[1].split(/[,\s/]+/).filter(Boolean).map(Number);
    return { r: p[0], g: p[1], b: p[2], a: p.length > 3 ? p[3] : 1 };
  }
  m = String(s).match(/^color\(srgb\s+([\d.]+)\s+([\d.]+)\s+([\d.]+)(?:\s*\/\s*([\d.]+))?\)$/);
  if (m) {
    return {
      r: Number(m[1]) * 255,
      g: Number(m[2]) * 255,
      b: Number(m[3]) * 255,
      a: m[4] === undefined ? 1 : Number(m[4]),
    };
  }
  return null;
}

// 把 fg 叠到 bg 上（alpha 合成）。
function over(fg, bg) {
  if (!fg) return bg;
  if (!bg) return fg;
  return {
    r: fg.r * fg.a + bg.r * (1 - fg.a),
    g: fg.g * fg.a + bg.g * (1 - fg.a),
    b: fg.b * fg.a + bg.b * (1 - fg.a),
    a: 1,
  };
}

// WCAG 相对亮度。
function relLum(c) {
  const f = (v) => {
    v = v / 255;
    return v <= 0.03928 ? v / 12.92 : Math.pow((v + 0.055) / 1.055, 2.4);
  };
  return 0.2126 * f(c.r) + 0.7152 * f(c.g) + 0.0722 * f(c.b);
}

// 对比度比值。
function contrast(a, b) {
  const l1 = relLum(a), l2 = relLum(b);
  const hi = Math.max(l1, l2), lo = Math.min(l1, l2);
  return (hi + 0.05) / (lo + 0.05);
}

function rgbStr(c) {
  return 'rgb(' + Math.round(c.r) + ',' + Math.round(c.g) + ',' + Math.round(c.b) + ')';
}

module.exports = { parseColor, over, relLum, contrast, rgbStr };

// 页面内执行：给定选择器，返回 {name, missing, fg, bg, ratio}。
// 需要以字符串形式注入浏览器，所以这里再导出一份自包含的实现。
module.exports.IN_PAGE_SOURCE = `
function __probeColors(pairs) {
  function parse(s) {
    if (!s) return null;
    var m = String(s).match(/^rgba?\\(([^)]+)\\)$/);
    if (m) {
      var p = m[1].split(/[,\\s/]+/).filter(Boolean).map(Number);
      return { r: p[0], g: p[1], b: p[2], a: p.length > 3 ? p[3] : 1 };
    }
    m = String(s).match(/^color\\(srgb\\s+([\\d.]+)\\s+([\\d.]+)\\s+([\\d.]+)(?:\\s*\\/\\s*([\\d.]+))?\\)$/);
    if (m) {
      return { r: Number(m[1]) * 255, g: Number(m[2]) * 255, b: Number(m[3]) * 255, a: m[4] === undefined ? 1 : Number(m[4]) };
    }
    return null;
  }
  function over(fg, bg) {
    if (!fg) return bg;
    if (!bg) return fg;
    return {
      r: fg.r * fg.a + bg.r * (1 - fg.a),
      g: fg.g * fg.a + bg.g * (1 - fg.a),
      b: fg.b * fg.a + bg.b * (1 - fg.a),
      a: 1,
    };
  }
  var out = [];
  for (var i = 0; i < pairs.length; i++) {
    var name = pairs[i][0], sel = pairs[i][1];
    var el = document.querySelector(sel);
    if (!el) { out.push({ name: name, missing: true }); continue; }
    var cs = getComputedStyle(el);
    // 从 body 到该元素（含自身）自上而下合成所有背景
    var stack = [], p = el, n = 0;
    while (p && n++ < 20) { stack.push(p); p = p.parentElement; }
    stack.reverse();
    var base = { r: 255, g: 255, b: 255, a: 1 };
    for (var k = 0; k < stack.length; k++) {
      var c = parse(getComputedStyle(stack[k]).backgroundColor);
      if (c && c.a > 0) base = over(c, base);
    }
    var fg = over(parse(cs.color), base);
    out.push({
      name: name,
      fg: 'rgb(' + Math.round(fg.r) + ',' + Math.round(fg.g) + ',' + Math.round(fg.b) + ')',
      bg: 'rgb(' + Math.round(base.r) + ',' + Math.round(base.g) + ',' + Math.round(base.b) + ')',
      fgRaw: cs.color,
      bgRaw: cs.backgroundColor,
    });
  }
  return out;
}
`;
