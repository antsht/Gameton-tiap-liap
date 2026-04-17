const API_BASE = '/api';

const btnStart = document.getElementById('btn-start');
const btnStop = document.getElementById('btn-stop');
const logContainer = document.getElementById('log-container');
const canvas = document.getElementById('arena-canvas');
const ctx = canvas.getContext('2d');

let isRunning = false;
let scale = 1;
let offsetX = 0;
let offsetY = 0;
let isDragging = false;
let startDragX = 0;
let startDragY = 0;
const BASE_CELL_SIZE = 3;

// UI Setup
btnStart.addEventListener('click', async () => {
    await fetch(`${API_BASE}/start`, { method: 'POST' });
    updateControls(true);
});

btnStop.addEventListener('click', async () => {
    await fetch(`${API_BASE}/stop`, { method: 'POST' });
    updateControls(false);
});



function updateControls(running) {
    isRunning = running;
    btnStart.disabled = running;
    btnStop.disabled = !running;
    document.getElementById('val-state').textContent = running ? 'ON' : 'OFF';
    document.getElementById('val-state').style.color = running ? 'var(--green)' : 'var(--red)';
}

// Map Controls Setup
const container = document.getElementById('canvas-container');

container.addEventListener('mousedown', (e) => {
    isDragging = true;
    startDragX = e.clientX - offsetX;
    startDragY = e.clientY - offsetY;
    container.style.cursor = 'grabbing';
});
window.addEventListener('mouseup', () => {
    isDragging = false;
    container.style.cursor = 'grab';
});
window.addEventListener('mousemove', (e) => {
    if(!isDragging) return;
    offsetX = e.clientX - startDragX;
    offsetY = e.clientY - startDragY;
});
container.addEventListener('wheel', (e) => {
    e.preventDefault();
    const zoomIntensity = 0.1;
    const wheel = e.deltaY < 0 ? 1 : -1;
    const zoom = Math.exp(wheel * zoomIntensity);
    
    const rect = canvas.getBoundingClientRect();
    const mouseX = e.clientX - rect.left;
    const mouseY = e.clientY - rect.top;

    offsetX = mouseX - (mouseX - offsetX) * zoom;
    offsetY = mouseY - (mouseY - offsetY) * zoom;
    scale *= zoom;
}, {passive: false});

document.getElementById('btn-zoom-in').onclick = () => { offsetX = canvas.width/2 - (canvas.width/2 - offsetX) * 1.2; offsetY = canvas.height/2 - (canvas.height/2 - offsetY) * 1.2; scale *= 1.2; };
document.getElementById('btn-zoom-out').onclick = () => { offsetX = canvas.width/2 - (canvas.width/2 - offsetX) / 1.2; offsetY = canvas.height/2 - (canvas.height/2 - offsetY) / 1.2; scale /= 1.2; };
document.getElementById('btn-zoom-reset').onclick = () => { scale = 1; offsetX = 0; offsetY = 0; };

async function fetchState() {
    try {
        const res = await fetch(`${API_BASE}/state`);
        const data = await res.json();
        
        if (data.IsRunning !== isRunning) updateControls(data.IsRunning);
        if (data.Arena) {
            updateDashboard(data.Arena);
            renderMap(data.Arena);
        }
    } catch(err) {
        // console.error(err);
    }
}

async function fetchLogs() {
    try {
        const res = await fetch(`${API_BASE}/logs`);
        const logs = await res.json();
        
        if (logs && logs.length > 0) {
            logs.forEach(l => {
                const div = document.createElement('div');
                div.className = 'log-line';
                div.textContent = l;
                logContainer.appendChild(div);
            });
            logContainer.scrollTop = logContainer.scrollHeight;
        }
    } catch(err) {}
}

function updateDashboard(arena) {
    document.getElementById('val-turn').textContent = arena.turnNo || 0;
    document.getElementById('val-my-plantations').textContent = arena.plantations ? arena.plantations.length : 0;
    document.getElementById('val-enemy-plantations').textContent = arena.enemy ? arena.enemy.length : 0;
    document.getElementById('val-beavers').textContent = arena.beavers ? arena.beavers.length : 0;
    document.getElementById('val-constructions').textContent = arena.construction ? arena.construction.length : 0;
    
    if (arena.plantationUpgrades) {
        document.getElementById('val-upgrade-pts').textContent = arena.plantationUpgrades.points;
        document.getElementById('val-upgrade-ttl').textContent = arena.plantationUpgrades.turnsUntilPoints;
        const tiersDiv = document.getElementById('upgrade-tiers');
        tiersDiv.innerHTML = '';
        if(arena.plantationUpgrades.tiers) {
            arena.plantationUpgrades.tiers.forEach(t => {
                tiersDiv.innerHTML += `<div><small>${t.name}</small>: ${t.current}/${t.max}</div>`;
            });
        }
    }

    const meteoList = document.getElementById('meteo-list');
    meteoList.innerHTML = '';
    if (arena.meteoForecasts && arena.meteoForecasts.length > 0) {
        arena.meteoForecasts.forEach(m => {
            meteoList.innerHTML += `<li>${m.kind} in ${m.turnsUntil} (pos: ${m.position ? m.position.join(',') : '?'})</li>`;
        });
    } else {
        meteoList.innerHTML = '<li>Clear weather</li>';
    }
}

function renderMap(arena) {
    if (!arena.size) return;
    
    const mapW = arena.size[0];
    const mapH = arena.size[1];
    const cs = BASE_CELL_SIZE;
    
    if (canvas.width !== container.clientWidth || canvas.height !== container.clientHeight) {
        canvas.width = container.clientWidth;
        canvas.height = container.clientHeight;
    }

    ctx.setTransform(1, 0, 0, 1, 0, 0); // reset
    ctx.fillStyle = '#0f111a';
    ctx.fillRect(0, 0, canvas.width, canvas.height);

    ctx.translate(offsetX, offsetY);
    ctx.scale(scale, scale);

    // X,Y multiplier highlighting (cells with coords %7==0 get 1.5x points)
    ctx.fillStyle = 'rgba(255,255,255,0.03)';
    for(let x=0; x<mapW; x+=7) {
        for(let y=0; y<mapH; y+=7) {
            ctx.fillRect(x*cs, y*cs, cs, cs);
        }
    }

    // Mountains
    ctx.fillStyle = '#475569';
    if(arena.mountains) {
        arena.mountains.forEach(m => {
            ctx.fillRect(m[0]*cs, m[1]*cs, cs, cs);
        });
    }

    // Terraformed cells
    if(arena.cells) {
        arena.cells.forEach(c => {
            const alpha = Math.max(0.2, c.terraformationProgress / 100.0);
            ctx.fillStyle = `rgba(16, 185, 129, ${alpha})`;
            ctx.fillRect(c.position[0]*cs, c.position[1]*cs, cs, cs);
        });
    }

    // Beavers
    ctx.fillStyle = '#f59e0b';
    if(arena.beavers) {
        arena.beavers.forEach(b => {
            ctx.fillRect(b.position[0]*cs, b.position[1]*cs, cs, cs);
        });
    }

    // Enemy
    ctx.fillStyle = '#ef4444'; 
    if(arena.enemy) {
        arena.enemy.forEach(e => {
            ctx.fillRect(e.position[0]*cs, e.position[1]*cs, cs, cs);
        });
    }

    // Us
    ctx.fillStyle = '#3b82f6';
    if(arena.plantations) {
        arena.plantations.forEach(p => {
            ctx.fillRect(p.position[0]*cs, p.position[1]*cs, cs, cs);
            if(p.isMain) {
                // Highlight main
                ctx.strokeStyle = '#fff';
                ctx.lineWidth = 1;
                ctx.strokeRect(p.position[0]*cs-1, p.position[1]*cs-1, cs+2, cs+2);
            }
        });
    }

    // Construction
    ctx.fillStyle = '#a855f7'; 
    if(arena.construction) {
        arena.construction.forEach(c => {
            ctx.fillRect(c.position[0]*cs, c.position[1]*cs, cs, cs);
        });
    }

    // Meteo (Sandstorms)
    if(arena.meteoForecasts) {
        arena.meteoForecasts.forEach(m => {
            if(m.kind === 'sandstorm' && m.position) {
                ctx.fillStyle = 'rgba(234, 179, 8, 0.4)';
                ctx.beginPath();
                ctx.arc(m.position[0]*cs, m.position[1]*cs, m.radius*cs, 0, 2*Math.PI);
                ctx.fill();
            }
        });
    }
}

setInterval(() => {
    fetchState();
    fetchLogs();
}, 500);
