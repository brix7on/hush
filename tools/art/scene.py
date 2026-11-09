"""Фон панели: python3 scene.py пишет ../../cmd/hush/scene.png."""
import numpy as np
from PIL import Image
W,H=600,580
INK=(10,10,11); PAPER=(222,216,202)
y,x=np.mgrid[0:H,0:W].astype(float)
g=np.zeros((H,W))
hz=388
# небо: тьма сверху, к горизонту светлее
sky=np.clip((y-40)/(hz-40),0,1)**2.2*0.42
g=np.where(y<hz,sky,0)
# вода: тёмная, светлее к горизонту
water=np.clip(1-(y-hz)/(H-hz),0,1)**3*0.30
g=np.where(y>=hz,water,g)
# огромная сфера, наполовину в воде, но без отражения
cx,cy,R=470,300,128
d=np.hypot(x-cx,y-cy)
inside=(d<R)&(y<hz+40)
lx,ly=-0.55,-0.6
nz=np.sqrt(np.clip(1-((x-cx)/R)**2-((y-cy)/R)**2,0,1))
sh=np.clip(((x-cx)/R)*lx+((y-cy)/R)*ly+nz*0.58,0,1)
g=np.where(inside&(y<hz),0.08+0.85*sh**1.4,g)
# линия горизонта режет сферу: нижняя часть уходит в воду, становится тёмной полосами
band=inside&(y>=hz)
g=np.where(band,np.where(((y-hz)//3)%2==0,0.55*sh,0.02),g)
# вместо отражения — лестница, уходящая вниз под воду
for i in range(9):
    sx0=cx-46+i*7; sx1=cx+46-i*7
    sy=hz+12+i*18
    step=(x>=sx0)&(x<=sx1)&(y>=sy)&(y<sy+6)
    g=np.where(step,0.92-i*0.08,g)
    riser=(x>=sx0)&(x<=sx1)&(y>=sy+6)&(y<sy+18)
    g=np.where(riser,0.18,g)
# застывшие круги звука — эллипсы, висящие в воздухе над водой
for k,(rx,ry) in enumerate([(70,9),(110,14),(150,19),(196,25)]):
    e=np.abs(((x-cx)/rx)**2+((y-(hz-6-k*4))/ry)**2-1)
    ring=(e<0.035+k*0.004)&~inside
    g=np.where(ring,0.95,g)
# парящие камни — поднимаются вверх, тают
rng=np.random.default_rng(7)
for i in range(16):
    px=rng.uniform(300,590); py=rng.uniform(40,hz-40)
    s=rng.uniform(4,15)*(0.4+py/hz)
    a=rng.uniform(0,np.pi)
    u=(x-px)*np.cos(a)+(y-py)*np.sin(a); v=-(x-px)*np.sin(a)+(y-py)*np.cos(a)
    m=(np.abs(u)<s)&(np.abs(v)<s*0.6)&(np.hypot(x-cx,y-cy)>R+6)
    g=np.where(m,np.clip(0.75-0.5*(v/(s*0.6)),0,1)*(0.4+py/hz*0.6),g)
# висящая в небе вторая луна — только контур, без заливки
d2=np.hypot(x-360,y-120)
g=np.where(np.abs(d2-26)<1.2,0.9,g)
# звёзды
for i in range(40):
    sx,sy=int(rng.uniform(0,W)),int(rng.uniform(0,hz*0.55))
    if np.hypot(sx-cx,sy-cy)>R+4: g[sy,sx]=1.0
# Bayer 8x8
b4=np.array([[0,8,2,10],[12,4,14,6],[3,11,1,9],[15,7,13,5]])
B=np.block([[4*b4,4*b4+2],[4*b4+3,4*b4+1]])/64+1/128
th=B[(y.astype(int))%8,(x.astype(int))%8]
bit=g>th
img=np.zeros((H,W,3),np.uint8); img[:]=INK; img[bit]=PAPER
Image.fromarray((bit*255).astype(np.uint8)).convert('1').save('../../cmd/hush/scene.png',optimize=True)
