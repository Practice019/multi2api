const http=require('http');
// 按**请求次数**轮换四种形态（不依赖任何被转发的内容 —— 实测 query 不会转发到上游）
const SEQ = [
  { name:'last',   frames:['{"choices":[{"delta":{"content":"你好"}}]}','{"choices":[{"delta":{}}],"usage":{"prompt_tokens":11,"completion_tokens":22,"total_tokens":33}}'] },
  { name:'second', frames:['{"choices":[{"delta":{"content":"hi"}}]}','{"choices":[{"delta":{}}],"usage":{"prompt_tokens":7,"completion_tokens":11,"total_tokens":18}}','{"choices":[{"delta":{}}]}'] },
  { name:'none',   frames:['{"choices":[{"delta":{"content":"这是一段用来估算的中文输出内容，长度足够"}}]}','[DONE]'] },
  { name:'empty',  frames:[] }
];
let n = 0;
http.createServer((req,res)=>{
  const item = SEQ[n % SEQ.length];
  n++;
  res.writeHead(200,{'Content-Type':'text/event-stream','Cache-Control':'no-cache','X-Case':item.name});
  let i=0;
  const tick=()=>{
    if(i>=item.frames.length){ res.end(); return; }
    res.write('data: '+item.frames[i]+'\n\n');
    i++;
    setTimeout(tick, 120);
  };
  tick();
}).listen(7903, ()=>console.log('fake upstream on 7903 (rotating by request count)'));