package crm

// Word lists for the generator. Company names are made up from parts so
// none of them is a real company.

var enPrefixes = combos("Blue", "North", "Bright", "Silver", "Red", "Green", "Clear", "Swift", "Iron", "Cedar",
	"Maple", "Harbor", "Summit", "River", "Stone", "Pixel", "Quantum", "Vertex", "Nimbus", "Orbit",
	"Amber", "Coral", "Falcon", "Granite", "Lumen", "Meadow", "Onyx", "Prairie", "Tidal", "Willow")

var enSuffixes = combos("wave", "path", "works", "labs", "soft", "logic", "data", "cloud", "line", "grid",
	"forge", "point", "bridge", "craft", "scale", "stack", "field", "gate", "mark", "sync")

var qualifiers = []string{"Systems", "Health", "Logistics", "Analytics", "Media", "Energy"}

var koPrefixes = []combo{
	{"한빛", "hanbit"}, {"누리", "nuri"}, {"새솔", "saesol"}, {"다온", "daon"}, {"가람", "garam"},
	{"하늘", "haneul"}, {"바른", "bareun"}, {"온새", "onsae"}, {"미르", "mir"}, {"라온", "raon"},
	{"해솔", "haesol"}, {"초록", "chorok"}, {"푸른", "pureun"}, {"별빛", "byeolbit"}, {"아라", "ara"},
	{"나래", "narae"}, {"다솜", "dasom"}, {"한결", "hangyeol"}, {"이음", "ieum"}, {"마루", "maru"},
}

var koSuffixes = []combo{
	{"소프트", "soft"}, {"테크", "tech"}, {"데이터", "data"}, {"시스템즈", "systems"}, {"랩스", "labs"},
	{"네트웍스", "networks"}, {"솔루션", "solution"}, {"바이오", "bio"}, {"에너지", "energy"}, {"물산", "trading"},
}

type koName struct {
	native    string
	roman     string
	syllables []string
}

var koFamilies = []koName{
	{native: "김", roman: "Kim"}, {native: "이", roman: "Lee"}, {native: "박", roman: "Park"},
	{native: "최", roman: "Choi"}, {native: "정", roman: "Jung"}, {native: "강", roman: "Kang"},
	{native: "조", roman: "Cho"}, {native: "윤", roman: "Yoon"}, {native: "장", roman: "Jang"},
	{native: "임", roman: "Lim"}, {native: "한", roman: "Han"}, {native: "오", roman: "Oh"},
	{native: "서", roman: "Seo"}, {native: "신", roman: "Shin"}, {native: "권", roman: "Kwon"},
}

var koGivens = []koName{
	{native: "재훈", syllables: []string{"Jae", "hoon"}}, {native: "민준", syllables: []string{"Min", "jun"}},
	{native: "서연", syllables: []string{"Seo", "yeon"}}, {native: "지우", syllables: []string{"Ji", "woo"}},
	{native: "하은", syllables: []string{"Ha", "eun"}}, {native: "도윤", syllables: []string{"Do", "yoon"}},
	{native: "수빈", syllables: []string{"Su", "bin"}}, {native: "예준", syllables: []string{"Ye", "jun"}},
	{native: "지민", syllables: []string{"Ji", "min"}}, {native: "현우", syllables: []string{"Hyun", "woo"}},
	{native: "은지", syllables: []string{"Eun", "ji"}}, {native: "성민", syllables: []string{"Sung", "min"}},
	{native: "유진", syllables: []string{"Yu", "jin"}}, {native: "동현", syllables: []string{"Dong", "hyun"}},
	{native: "소연", syllables: []string{"So", "yeon"}}, {native: "태윤", syllables: []string{"Tae", "yoon"}},
	{native: "다은", syllables: []string{"Da", "eun"}}, {native: "준서", syllables: []string{"Jun", "seo"}},
	{native: "채원", syllables: []string{"Chae", "won"}}, {native: "승현", syllables: []string{"Seung", "hyun"}},
}

type enGiven struct{ name, nick string }

var enGivens = []enGiven{
	{"James", "Jim"}, {"Mary", ""}, {"Robert", "Bob"}, {"Patricia", "Pat"}, {"Michael", "Mike"},
	{"Jennifer", "Jen"}, {"William", "Bill"}, {"Linda", ""}, {"David", "Dave"}, {"Elizabeth", "Liz"},
	{"Richard", "Rick"}, {"Susan", "Sue"}, {"Joseph", "Joe"}, {"Jessica", "Jess"}, {"Thomas", "Tom"},
	{"Sarah", ""}, {"Daniel", "Dan"}, {"Karen", ""}, {"Matthew", "Matt"}, {"Emily", ""},
	{"Andrew", "Andy"}, {"Olivia", "Liv"}, {"Christopher", "Chris"}, {"Sophia", ""}, {"Benjamin", "Ben"},
}

var enFamilies = []string{"Smith", "Johnson", "Williams", "Brown", "Jones", "Garcia", "Miller", "Davis",
	"Rodriguez", "Martinez", "Wilson", "Anderson", "Taylor", "Thomas", "Moore", "Jackson", "Martin",
	"Thompson", "White", "Harris", "Clark", "Lewis", "Walker", "Young", "Allen"}

var industries = []string{"SaaS", "Fintech", "Healthcare", "Logistics", "Retail", "Manufacturing",
	"Media", "Education", "Energy", "Biotech"}

var titles = []string{
	"Software Engineer", "Senior Software Engineer", "Engineering Manager", "Director of Engineering",
	"VP Engineering", "CTO", "Product Manager", "Senior Product Manager", "Director of Product",
	"VP Product", "Account Executive", "Sales Manager", "Sales Director", "VP Sales", "CRO",
	"Marketing Manager", "Head of Marketing", "CMO", "Operations Manager", "COO",
	"Data Analyst", "Head of Data", "IT Manager", "Procurement Lead", "CEO", "Founder",
}

// tracks order titles by seniority within one function.
var tracks = [][]string{
	{"Software Engineer", "Senior Software Engineer", "Engineering Manager", "Director of Engineering", "VP Engineering", "CTO"},
	{"Product Manager", "Senior Product Manager", "Director of Product", "VP Product"},
	{"Account Executive", "Sales Manager", "Sales Director", "VP Sales", "CRO"},
	{"Marketing Manager", "Head of Marketing", "CMO"},
	{"Operations Manager", "COO"},
	{"Data Analyst", "Head of Data"},
}

var consentSources = []string{"webinar signup", "form: pricing page", "event badge scan", "double opt-in email"}

var notes = []string{
	"Prefers email over calls.", "Met at SaaS Connect 2026.", "Decision maker for tooling budget.",
	"Introduced by partner team.", "Asked for a security review packet.", "Out of office until next month.",
	"Champion for the pilot.", "Evaluating two other vendors.",
}

var topics = []string{"the pilot", "pricing", "renewal", "the security review", "onboarding",
	"the integration", "quarterly goals", "the expansion plan"}

var activityBodies = map[string][]string{
	"email":   {"Sent follow-up about %s.", "Shared the deck on %s.", "Answered questions on %s."},
	"call":    {"Intro call about %s.", "Check-in call on %s.", "Call to unblock %s."},
	"meeting": {"Meeting to review %s.", "Workshop on %s.", "Quarterly review covering %s."},
}

func combos(words ...string) []combo {
	out := make([]combo, len(words))
	for i, w := range words {
		out[i] = combo{w, w}
	}
	return out
}
