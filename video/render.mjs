import puppeteer from 'puppeteer-core';
import fs from 'fs';
import path from 'path';

const HERE = path.dirname(new URL(import.meta.url).pathname);
const CHROME = '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome';
const MODE = process.argv[2] || 'verify';
const FPS = 30;
const TL = JSON.parse(fs.readFileSync(path.join(HERE,'timeline.js'),'utf8').replace(/^window\.TIMELINE = /,'').replace(/;\s*$/,''));
const TOTAL = TL.total;
const sleep = ms => new Promise(r=>setTimeout(r,ms));

const browser = await puppeteer.launch({
  executablePath: CHROME, headless: 'new',
  args:['--no-sandbox','--hide-scrollbars','--force-color-profile=srgb']
});
const page = await browser.newPage();
await page.setViewport({width:1920,height:1080,deviceScaleFactor:1});
await page.goto('file://'+path.join(HERE,'index.html')+'?render=1',{waitUntil:'networkidle0'});
await page.evaluate(()=>document.fonts.ready);
await sleep(300);

// CDP screencast — the only capture path that isn't frozen by the canvas layer under headless.
const cdp = await page.createCDPSession();
let last=null, frameCount=0;
cdp.on('Page.screencastFrame', async e=>{
  last=e.data; frameCount++;
  try{ await cdp.send('Page.screencastFrameAck',{sessionId:e.sessionId}); }catch{}
});
await cdp.send('Page.startScreencast',{format:'png', everyNthFrame:1, maxWidth:1920, maxHeight:1080});

// seek, then wait until the compositor goes quiet and take the last (settled) frame,
// which discards any stale in-flight frame from the previous seek.
async function grab(t, file){
  await page.evaluate(tt=>window.seek(tt), t);
  const startCount = frameCount;
  const start = Date.now();
  let quiet = 0, lastSeen = frameCount;
  while(Date.now()-start < 800){
    await sleep(30);
    if(frameCount !== lastSeen){ lastSeen = frameCount; quiet = 0; }
    else { quiet += 30; if(frameCount > startCount && quiet >= 90) break; }
  }
  fs.writeFileSync(file, Buffer.from(last,'base64'));
}

if(MODE==='verify'){
  const outdir = path.join(HERE,'verify'); fs.mkdirSync(outdir,{recursive:true});
  const marks = [
    ['s0-poster',1.6],['s1-pain',6.0],['s2-cold',13.5],['trans',20.8],
    ['s3-writes',22.5],['s4-wedge',27.8],['s5-pipeline',36.5],['s6-graph',43.5],
    ['s7-diff',52.5],['s8-cta',61.5]
  ];
  for(const [name,t] of marks){ await grab(t, path.join(outdir, name+'.png')); console.log('verify',name,t); }
} else {
  const outdir = path.join(HERE,'frames'); fs.mkdirSync(outdir,{recursive:true});
  const n = Math.ceil(TOTAL*FPS);
  const t0=Date.now();
  for(let f=0; f<n; f++){
    await grab(f/FPS, path.join(outdir, String(f).padStart(5,'0')+'.png'));
    if(f%60===0){ const el=(Date.now()-t0)/1000; console.log(`frame ${f}/${n} (${(f/FPS).toFixed(1)}s) · ${el.toFixed(0)}s elapsed`); }
  }
  console.log('done', n, 'frames');
}
await cdp.send('Page.stopScreencast');
await browser.close();
