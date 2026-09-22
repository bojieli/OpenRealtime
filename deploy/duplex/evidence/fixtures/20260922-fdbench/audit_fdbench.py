import hashlib,json,pathlib,wave,datetime
root=pathlib.Path('.runtime/fd-bench/dataset')
report={'time':datetime.datetime.now(datetime.timezone.utc).isoformat(),'timestamp_rate':16000,'scope':'Local WAV headers, payload lengths, annotations and SHA256; not semantic annotation or license validation','conditions':{}}
for directory in sorted(root.iterdir()):
    if not directory.is_dir(): continue
    cases=[]
    for path in sorted(directory.glob('*.wav')):
        issues=[]
        try:
            with wave.open(str(path)) as f:
                rate,channels,width,frames=f.getframerate(),f.getnchannels(),f.getsampwidth(),f.getnframes()
                pcm=f.readframes(frames)
                if len(pcm)!=frames*channels*width: issues.append('truncated PCM payload')
            annotation=path.with_suffix('.timestamps')
            turns=json.loads(annotation.read_text())
            if not turns: issues.append('no turns')
            previous=0
            for t in turns:
                if not 0<=t['start']<t['end']<=frames/rate*16000+1: issues.append('timestamp outside audio or reversed')
                if t['start']<previous: issues.append('overlapping annotation')
                previous=t['end']
            cases.append({'id':path.stem,'rate':rate,'channels':channels,'sample_width':width,'frames':frames,'turns':len(turns),'duration_s':frames/rate,'wav_sha256':hashlib.file_digest(path.open('rb'),'sha256').hexdigest() if hasattr(hashlib,'file_digest') else hashlib.sha256(path.read_bytes()).hexdigest(),'timestamps_sha256':hashlib.sha256(annotation.read_bytes()).hexdigest(),'issues':issues})
        except Exception as e:
            cases.append({'id':path.stem,'issues':[str(e)]})
    if cases:
        report['conditions'][directory.name]={'conversations':len(cases),'duration_hours':sum(c.get('duration_s',0) for c in cases)/3600,'cases_with_issues':sum(bool(c['issues']) for c in cases),'cases':cases}
        print(directory.name,len(cases),report['conditions'][directory.name]['cases_with_issues'],flush=True)
pathlib.Path('.runtime/duplex-plan/results/fixtures/fdbench-inventory.json').write_text(json.dumps(report,indent=2)+'\n')
