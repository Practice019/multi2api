// lib_contrast_audit.js —— 页内对比度巡检函数（独立文件，便于 node --check 单独校验）。
//
// # 为什么用 function.toString() 而不是内嵌字符串模板
// 内嵌模板要在“字符串里写引号与反斜杠”和“模板里再套模板”之间反复转义，
// 本仓已经为这件事交过学费（generator 的 String.raw 里一个裸反引号 ⇒ 模板提前终止 ⇒
// 陈旧产物照旧全绿）。写成**真函数**再由 toString() 序列化，转义问题从根上消失，
// 而且这份逻辑还能被 node --check 单独校验。
'use strict';

function __auditContrast(opts) {
  var MIN = (opts && opts.min) || 4.5;
  function parse(s) {
    if (!s) return null;
    var m = String(s).match(/^rgba?\(([^)]+)\)$/);
    if (m) { var p = m[1].split(/[,\s/]+/).filter(Boolean).map(Number); return { r: p[0], g: p[1], b: p[2], a: p.length > 3 ? p[3] : 1 }; }
    m = String(s).match(/^color\(srgb\s+([\d.]+)\s+([\d.]+)\s+([\d.]+)(?:\s*\/\s*([\d.]+))?\)$/);
    if (m) { return { r: Number(m[1]) * 255, g: Number(m[2]) * 255, b: Number(m[3]) * 255, a: m[4] === undefined ? 1 : Number(m[4]) }; }
    return null;
  }
  function over(fg, bg) {
    if (!fg) return bg; if (!bg) return fg;
    return { r: fg.r * fg.a + bg.r * (1 - fg.a), g: fg.g * fg.a + bg.g * (1 - fg.a), b: fg.b * fg.a + bg.b * (1 - fg.a), a: 1 };
  }
  function lum(c) {
    function f(v) { v = v / 255; return v <= 0.03928 ? v / 12.92 : Math.pow((v + 0.055) / 1.055, 2.4); }
    return 0.2126 * f(c.r) + 0.7152 * f(c.g) + 0.0722 * f(c.b);
  }
  function ratio(a, b) { var l1 = lum(a), l2 = lum(b), hi = Math.max(l1, l2), lo = Math.min(l1, l2); return (hi + 0.05) / (lo + 0.05); }
  function cssPath(el) {
    var parts = [];
    for (var n = el, d = 0; n && n.nodeType === 1 && d < 4; n = n.parentElement, d++) {
      var s = n.tagName.toLowerCase();
      if (n.id) { parts.unshift(s + '#' + n.id); break; }
      var cls = (n.className && typeof n.className === 'string') ? n.className.trim().split(/\s+/).slice(0, 2) : [];
      if (cls.length) s += '.' + cls.join('.');
      parts.unshift(s);
    }
    return parts.join('>');
  }
  var out = [];
  var all = document.querySelectorAll('body *');
  for (var i = 0; i < all.length; i++) {
    var el = all[i];
    if (el.getAttribute && el.getAttribute('aria-hidden') === 'true') continue;
    var cs = getComputedStyle(el);
    if (cs.display === 'none' || cs.visibility === 'hidden' || parseFloat(cs.opacity) === 0) continue;
    if (!el.offsetWidth && !el.offsetHeight) continue;
    var text = '';
    for (var k = 0; k < el.childNodes.length; k++) {
      var nd = el.childNodes[k];
      if (nd.nodeType === 3) text += nd.nodeValue;
    }
    text = text.replace(/\s+/g, ' ').trim();
    if (!text) continue;
    var stack = [], p = el, guard = 0;
    while (p && guard++ < 30) { stack.push(p); p = p.parentElement; }
    stack.reverse();
    var base = { r: 255, g: 255, b: 255, a: 1 };
    for (var s2 = 0; s2 < stack.length; s2++) {
      var c = parse(getComputedStyle(stack[s2]).backgroundColor);
      if (c && c.a > 0) base = over(c, base);
    }
    var fgRaw = parse(cs.color);
    if (!fgRaw) continue;
    var fg = over(fgRaw, base);
    var cr = ratio(fg, base);
    var size = parseFloat(cs.fontSize) || 13;
    var weight = parseInt(cs.fontWeight, 10) || 400;
    var large = size >= 24 || (size >= 18.66 && weight >= 700);
    var need = large ? 3 : MIN;
    if (cr < need) {
      out.push({
        sel: cssPath(el), text: text.slice(0, 36),
        fg: 'rgb(' + Math.round(fg.r) + ',' + Math.round(fg.g) + ',' + Math.round(fg.b) + ')',
        bg: 'rgb(' + Math.round(base.r) + ',' + Math.round(base.g) + ',' + Math.round(base.b) + ')',
        cr: Math.round(cr * 100) / 100, need: need, size: size, weight: weight,
        cls: (typeof el.className === 'string' ? el.className : '')
      });
    }
  }
  return out;
}

module.exports = { name: '__auditContrast', source: __auditContrast.toString() };
