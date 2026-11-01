-- Protocol v1. All validation and JSON encoding precede the first write.
-- KEYS: state, ready, leases. ARGV[1]: JSON command. Single Redis only.
local now=0
local function reply(code, state)
    return cjson.encode({code=code, state=state, now=now})
end
-- Worst-case Redis cost of one active run: 7MiB encoded state plus index and
-- allocator margin. Not a measured figure; deliberately counts used bytes twice.
local active_reserve=8388608
local function integer(n, lo, hi)
    return type(n)=='number' and n==math.floor(n) and n>=lo and n<=hi
end
local function identifier(s)
    return type(s)=='string' and #s>0 and #s<=128 and string.match(s, '^[%w_-]+$')~=nil
end
if #KEYS~=3 or #ARGV~=1 or KEYS[1]==KEYS[2] or KEYS[1]==KEYS[3] or KEYS[2]==KEYS[3] then
    return reply('INVALID')
end
if #ARGV[1]>8388608 then return reply('CAPACITY') end
local ok, cmd=pcall(cjson.decode, ARGV[1])
if not ok or type(cmd)~='table' or not identifier(cmd.id) then return reply('INVALID') end
local allowed={create=true,read=true,claim=true,renew=true,begin=true,commit=true,complete=true,cancel=true,checkpoint=true}
if not allowed[cmd.action] then return reply('INVALID') end
local types={'string','zset','zset'}
for i=1,3 do
    local kind=redis.call('TYPE',KEYS[i]).ok
    if kind~='none' and kind~=types[i] then return reply('WRONGTYPE') end
end
local clock=redis.call('TIME')
now=tonumber(clock[1])*1000+math.floor(tonumber(clock[2])/1000)
local raw=redis.call('GET',KEYS[1])
local s=nil
if raw then
    local valid
    valid,s=pcall(cjson.decode,raw)
    if not valid or type(s)~='table' or type(s.schema)~='number' or s.id~=cmd.id then return reply('CORRUPT') end
    if s.schema~=1 then return reply('UNKNOWN_SCHEMA') end
end
if cmd.action=='read' then
    if not s then return reply('NOT_FOUND') end
    return reply('OK',s)
end
if cmd.action=='create' then
    if s then
        if s.request==cmd.request and s.deadline==cmd.deadline then return reply('EXISTS',s) end
        return reply('CONFLICT')
    end
    if type(cmd.request)~='string' or #cmd.request>16384 or not integer(cmd.deadline,now+1,now+180000) then
        return reply('INVALID')
    end
    if not integer(cmd.max_active,1,1024) then return reply('INVALID') end
    -- Indexes hold exactly the active runs, so claim and takeover cannot change the sum.
    local active=redis.call('ZCARD',KEYS[2])+redis.call('ZCARD',KEYS[3])
    if active>=cmd.max_active then return reply('OVER_CAPACITY') end
    -- Keep room for admitted runs to finish before noeviction starts refusing writes.
    local memory=redis.call('INFO','memory')
    local used=tonumber(string.match(memory,'used_memory:(%d+)'))
    local limit=tonumber(string.match(memory,'maxmemory:(%d+)'))
    if not used or not limit or limit==0 or used+(active+1)*active_reserve>limit then
        return reply('OVER_MEMORY')
    end
    s={schema=1,id=cmd.id,request=cmd.request,created=now,deadline=cmd.deadline,
       status='READY',owner='',epoch=0,version=1,lease_until=0,cancelled=false,
       tokens=0,requests=0,model_calls=0,operations={},operation_count=0,result_bytes=0}
else
    if not s then
        -- An index entry without state would otherwise hold a capacity slot forever.
        if cmd.action=='claim' then
            redis.call('ZREM',KEYS[2],cmd.id)
            redis.call('ZREM',KEYS[3],cmd.id)
        end
        return reply('NOT_FOUND')
    end
    if s.status=='COMPLETED' or s.status=='CANCELLED' or s.status=='DEADLINE' then
        if cmd.action=='complete' and s.report==cmd.report then return reply('ALREADY',s) end
        return reply('TERMINAL',s)
    end
    if cmd.action=='cancel' then
        s.cancelled=true
        if s.status=='READY' then s.status='CANCELLED'; s.finished=now end
        s.version=s.version+1
    elseif cmd.action=='claim' then
        if not identifier(cmd.owner) then return reply('INVALID') end
        if s.status=='RUNNING' and s.lease_until>now then return reply('BUSY') end
        s.owner=cmd.owner; s.epoch=s.epoch+1; s.version=s.version+1
        s.status='RUNNING'; s.lease_until=now+15000
        -- A lost response is not free. Preserve the reservation and audit history.
        for _,op in pairs(s.operations) do
            if op.status=='IN_FLIGHT' then op.status='UNKNOWN' end
        end
    else
        if s.owner~=cmd.owner or s.epoch~=cmd.epoch or s.lease_until<=now then return reply('FENCED') end
        if cmd.action=='renew' then
            s.lease_until=now+15000
        else
            -- Replay acknowledgment is read-only and must never start another call.
            local op=nil
            if cmd.action=='begin' or cmd.action=='commit' then
                if not identifier(cmd.operation) then return reply('INVALID') end
                op=s.operations[cmd.operation]
                if cmd.action=='begin' and op then
                    if op.kind==cmd.kind and op.cost==cmd.cost and op.input==cmd.input then return reply('ALREADY',s) end
                    return reply('CONFLICT')
                end
                if cmd.action=='commit' and op and op.status=='COMMITTED' then
                    if op.result==cmd.result then return reply('ALREADY',s) end
                    return reply('CONFLICT')
                end
            end
            if s.version~=cmd.version then return reply('CONFLICT') end
            if cmd.action=='checkpoint' then
                if type(cmd.checkpoint)~='string' or #cmd.checkpoint>3145728 then return reply('CAPACITY') end
                s.checkpoint=cmd.checkpoint
            elseif cmd.action=='complete' then
                if cmd.checkpoint and #cmd.checkpoint>3145728 then return reply('CAPACITY') end
                                if type(cmd.report)~='string' or #cmd.report>65536 then return reply('INVALID') end
                if s.cancelled then s.status='CANCELLED'
                elseif s.deadline<=now then s.status='DEADLINE'
                else s.status='COMPLETED' end
                -- A cancelled/expired run cannot publish the proposed report.
                if s.status=='COMPLETED' then
                    s.report=cmd.report
                    if cmd.checkpoint then s.checkpoint=cmd.checkpoint end
                else
                    s.report=''
                    -- Never publish a completed checkpoint if cancellation/deadline won.
                end
                s.finished=now
            else
                if s.cancelled then return reply('CANCELLED') end
                if s.deadline<=now then return reply('DEADLINE') end
                if cmd.action=='begin' then
                    if cmd.kind~='model' and cmd.kind~='query' and cmd.kind~='tool' then return reply('INVALID') end
                    if type(cmd.input)~='string' or #cmd.input>65536 then return reply('INVALID') end
                    if not integer(cmd.cost,0,24000) then return reply('INVALID') end
                    if s.operation_count>=40 then return reply('CAPACITY') end
                    if cmd.kind=='model' then
                        if cmd.cost<1 then return reply('INVALID') end
                        if s.tokens+cmd.cost>24000 or s.model_calls>=12 then return reply('BUDGET') end
                        s.tokens=s.tokens+cmd.cost
                        s.model_calls=s.model_calls+1
                    elseif cmd.kind=='query' then
                        if cmd.cost~=1 then return reply('INVALID') end
                        if s.requests>=16 then return reply('BUDGET') end
                        s.requests=s.requests+1
                    elseif cmd.cost~=0 then return reply('INVALID') end
                    s.operations[cmd.operation]={kind=cmd.kind,cost=cmd.cost,input=cmd.input,status='IN_FLIGHT',epoch=s.epoch,started=now}
                    s.operation_count=s.operation_count+1
                elseif cmd.action=='commit' then
                    if not op or op.status~='IN_FLIGHT' or op.epoch~=s.epoch then return reply('CONFLICT') end
                    if type(cmd.result)~='string' then return reply('INVALID') end
                    if s.result_bytes+#cmd.result>2097152 then return reply('CAPACITY') end
                    op.result=cmd.result; op.status='COMMITTED'; op.finished=now
                    s.result_bytes=s.result_bytes+#cmd.result
                end
            end
            s.version=s.version+1
        end
    end
end
local encoded=cjson.encode(s)
if #encoded>7340032 then return reply('CAPACITY') end
local response=reply('OK',s)
-- Single state value keeps evidence, audit and business state together.
redis.call('SET',KEYS[1],encoded)
if s.status=='READY' then
    redis.call('ZADD',KEYS[2],s.created,s.id)
    redis.call('ZREM',KEYS[3],s.id)
elseif s.status=='RUNNING' then
    redis.call('ZREM',KEYS[2],s.id)
    redis.call('ZADD',KEYS[3],s.lease_until,s.id)
else
    redis.call('PEXPIRE',KEYS[1],604800000)
    redis.call('ZREM',KEYS[2],s.id)
    redis.call('ZREM',KEYS[3],s.id)
end
return response
