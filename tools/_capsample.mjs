// 用 JS 原版算标准样本
function capI(str, len) {
  let p = 2166136261;
  for (let i = 0; i < str.length; i++) {
    p = (p | 0) ^ str.charCodeAt(i);
    p = p + (((p << 1) | 0) + ((p << 4) | 0) + ((p << 7) | 0) + ((p << 8) | 0) + ((p << 24) | 0));
  }
  let u = p >>> 0;
  const next = () => { u = ((u | 0) ^ ((u << 13) | 0)) >>> 0; u = ((u | 0) ^ (u >>> 17)) >>> 0; u = ((u | 0) ^ ((u << 5) | 0)) >>> 0; return u; };
  let out = '';
  while (out.length < len) out += next().toString(16).padStart(8, '0');
  return out.substring(0, len);
}
const cases = [
  ['abc', 32], ['tokenabc1', 32], ['x', 8], ['hello world', 16],
  ['0123456789abcdef', 32], ['salt', 32], ['a', 32], ['', 32],
];
console.log('=== JS capI 标准样本 ===');
for (const [s, n] of cases) console.log('  capI(' + JSON.stringify(s) + ', ' + n + ') = "' + capI(s, n) + '"');
console.log('');
// 中间值，便于定位
let p = 2166136261;
const str = 'abc';
console.log('=== "abc" 逐步中间值 ===');
console.log('  初始 p = ' + p);
for (let i = 0; i < str.length; i++) {
  const p0 = (p | 0) ^ str.charCodeAt(i);
  const sh = ((p0 << 1) | 0) + ((p0 << 4) | 0) + ((p0 << 7) | 0) + ((p0 << 8) | 0) + ((p0 << 24) | 0);
  p = p0 + sh;
  console.log('  i=' + i + '  p0=' + p0 + '  shifted=' + sh + '  p=' + p);
}
console.log('  p >>> 0 = ' + (p >>> 0));
